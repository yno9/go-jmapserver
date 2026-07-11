package jmapserver

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
	jmap "git.sr.ht/~rockorager/go-jmap"
)

// PushSubscription is a browser Web Push subscription (RFC 8291), as returned
// by PushManager.subscribe() on the client.
type PushSubscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// SetVAPIDKeys enables Web Push sending. Generate a pair once per deployment
// with webpush.GenerateVAPIDKeys() and keep them stable — the public key is
// baked into every client subscription; rotating it invalidates all of them.
//
// subscriber is a contact (an email address or an https: URL) identifying the
// sender, put in the VAPID JWT's "sub" claim (RFC 8292). Some push services
// (Apple's web.push.apple.com in particular — Google's FCM is lenient) reject
// the send outright with 403 ("BadJwtToken") if this is empty or malformed.
// Pass a bare email address (e.g. "you@example.com"), NOT "mailto:you@..." —
// webpush-go prepends "mailto:" itself, so a caller-supplied prefix doubles up
// into "mailto:mailto:...", which is exactly the malformed subject Apple
// rejects (see github.com/SherClockHolmes/webpush-go/issues/81). Any leading
// "mailto:" is stripped defensively so either form works.
func (h *Hub) SetVAPIDKeys(public, private, subscriber string) {
	if len(subscriber) >= 7 && strings.EqualFold(subscriber[:7], "mailto:") {
		subscriber = subscriber[7:]
	}
	h.pushMu.Lock()
	h.vapidPublic = public
	h.vapidPrivate = private
	h.vapidSubscriber = subscriber
	h.pushMu.Unlock()
}

// VAPIDPublicKey returns the configured public key, served to clients so they
// can call PushManager.subscribe({applicationServerKey: ...}).
func (h *Hub) VAPIDPublicKey() string {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	return h.vapidPublic
}

// SetPersistDir enables disk persistence for push subscriptions (push_subs.json
// in dir) and loads any that already exist there. Optional — without calling
// this, subscriptions are memory-only and lost on restart.
func (h *Hub) SetPersistDir(dir string) {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	h.pushDir = dir
	b, err := os.ReadFile(h.pushSubsPath())
	if err != nil {
		return
	}
	var subs map[jmap.ID][]PushSubscription
	if json.Unmarshal(b, &subs) == nil {
		h.pushSubs = subs
	}
}

func (h *Hub) pushSubsPath() string {
	return filepath.Join(h.pushDir, "push_subs.json")
}

func (h *Hub) savePushSubsLocked() {
	if h.pushDir == "" {
		return
	}
	b, err := json.Marshal(h.pushSubs)
	if err != nil {
		return
	}
	os.WriteFile(h.pushSubsPath(), b, 0644) //nolint:errcheck
}

// PushSubscriptionCount returns the total number of registered subscriptions
// across all accounts (diagnostic use, e.g. cmd/pushtest).
func (h *Hub) PushSubscriptionCount() int {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	n := 0
	for _, subs := range h.pushSubs {
		n += len(subs)
	}
	return n
}

// AddPushSubscription registers a push subscription for accountID (a no-op if
// that endpoint is already registered for it).
func (h *Hub) AddPushSubscription(accountID jmap.ID, sub PushSubscription) {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	if h.pushSubs == nil {
		h.pushSubs = map[jmap.ID][]PushSubscription{}
	}
	for _, s := range h.pushSubs[accountID] {
		if s.Endpoint == sub.Endpoint {
			return
		}
	}
	h.pushSubs[accountID] = append(h.pushSubs[accountID], sub)
	h.savePushSubsLocked()
}

// RemovePushSubscription unregisters a push subscription by endpoint.
func (h *Hub) RemovePushSubscription(accountID jmap.ID, endpoint string) {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	h.removeSubscriptionLocked(accountID, endpoint)
	h.savePushSubsLocked()
}

func (h *Hub) removeSubscriptionLocked(accountID jmap.ID, endpoint string) {
	subs := h.pushSubs[accountID]
	next := subs[:0]
	for _, s := range subs {
		if s.Endpoint != endpoint {
			next = append(next, s)
		}
	}
	h.pushSubs[accountID] = next
}

// pushAll wakes every registered subscription, across every account this Hub
// serves — mirroring Notify's own SSE fan-out, which is already unscoped by
// account. The payload is deliberately empty: an identity can span multiple
// relays (mail + ActivityPub), so only the client's Service Worker — which
// knows about all of them — can compute the true unread total. This call's
// only job is to wake it while the page is backgrounded or fully closed.
func (h *Hub) pushAll() {
	h.pushMu.Lock()
	public, private, subscriber := h.vapidPublic, h.vapidPrivate, h.vapidSubscriber
	if public == "" || private == "" {
		h.pushMu.Unlock()
		return
	}
	type target struct {
		accountID jmap.ID
		sub       PushSubscription
	}
	var targets []target
	for accountID, subs := range h.pushSubs {
		for _, s := range subs {
			targets = append(targets, target{accountID, s})
		}
	}
	h.pushMu.Unlock()

	for _, t := range targets {
		h.sendOne(t.accountID, t.sub, public, private, subscriber)
	}
}

// sendOne sends to a single subscription. A malformed subscription (e.g. a
// corrupt key saved by a since-fixed bug, or one the browser mangled) can make
// the underlying crypto code panic rather than return an error — isolated in
// its own recover() so one bad entry can't abort the fan-out to everyone else.
func (h *Hub) sendOne(accountID jmap.ID, sub PushSubscription, public, private, subscriber string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[push] send to %s panicked: %v", accountID, r)
		}
	}()
	resp, err := webpush.SendNotification([]byte{}, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		VAPIDPublicKey:  public,
		VAPIDPrivateKey: private,
		Subscriber:      subscriber,
		TTL:             60,
	})
	if err != nil {
		log.Printf("[push] send to %s failed: %v", accountID, err)
		return
	}
	resp.Body.Close()
	log.Printf("[push] send to %s: %s", accountID, resp.Status)
	// 404/410 means the browser/OS dropped the subscription; stop retrying it.
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		h.pushMu.Lock()
		h.removeSubscriptionLocked(accountID, sub.Endpoint)
		h.savePushSubsLocked()
		h.pushMu.Unlock()
	}
}

// ── HTTP handlers ────────────────────────────────────────────────────────────

func servePushVapidKey(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(hub.VAPIDPublicKey())) //nolint:errcheck
	}
}

func servePushSubscribe(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		accountID, _ := r.Context().Value(ctxAccountID).(jmap.ID)
		var body struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		hub.AddPushSubscription(accountID, PushSubscription{
			Endpoint: body.Endpoint,
			P256dh:   body.Keys.P256dh,
			Auth:     body.Keys.Auth,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

func servePushUnsubscribe(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		accountID, _ := r.Context().Value(ctxAccountID).(jmap.ID)
		var body struct {
			Endpoint string `json:"endpoint"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		hub.RemovePushSubscription(accountID, body.Endpoint)
		w.WriteHeader(http.StatusNoContent)
	}
}
