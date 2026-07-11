// Package jmapserver provides a general-purpose JMAP server framework (RFC 8620/8621).
//
// A JMAP server is built by implementing the Handler interface and calling Serve:
//
//	type myHandler struct{ store *jmapserver.Store }
//
//	func (h *myHandler) Capabilities() []jmap.URI { ... }
//	func (h *myHandler) Accounts() []jmapserver.Account { ... }
//	func (h *myHandler) Handle(method string, args json.RawMessage) (any, error) { ... }
//
//	hub := jmapserver.NewHub()
//	jmapserver.Serve(cfg, handler, hub)
package jmapserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

type ctxKey int

const ctxAccountID ctxKey = 0

// Config is the HTTP server configuration.
type Config struct {
	ListenAddr string `json:"listen_addr,omitempty"`
	Password   string `json:"password,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	// AuthFunc authenticates a request. Returns the JMAP account ID and true on success.
	// If nil, falls back to Password (single global password, all accounts accessible).
	AuthFunc func(username, password string) (jmap.ID, bool) `json:"-"`
	// VAPID keypair for Web Push (see push.go). Generate once per deployment
	// with webpush.GenerateVAPIDKeys() and keep stable — rotating invalidates
	// every client's subscription. Leave both empty to disable Web Push.
	VapidPublicKey  string `json:"vapid_public_key,omitempty"`
	VapidPrivateKey string `json:"vapid_private_key,omitempty"`
	// VapidSubscriber is a contact identifying the sender, per RFC 8292 — a
	// bare email address (e.g. "you@example.com") or an https: URL. Some push
	// services (Apple's in particular) reject the send with 403 if this is
	// left empty. Required alongside the keys above for Web Push to actually
	// work, not just optional metadata. Do not include a "mailto:" prefix —
	// see Hub.SetVAPIDKeys.
	VapidSubscriber string `json:"vapid_subscriber,omitempty"`
}

// Account describes a JMAP account exposed by the server.
type Account struct {
	ID   jmap.ID
	Name string
}

// BlobHandler is optionally implemented by Handler to support blob upload/download.
// If the Handler also implements BlobHandler, /jmap/upload/ and /jmap/download/ endpoints are activated.
type BlobHandler interface {
	UploadBlob(contentType string, data []byte) string
	DownloadBlob(blobID string) (data []byte, ok bool)
}

// Handler is implemented by each protocol layer.
// The server calls these methods; the handler never touches HTTP or JMAP wire format.
type Handler interface {
	// Capabilities returns the JMAP URIs this server supports.
	// "urn:ietf:params:jmap:core" is added automatically.
	Capabilities() []jmap.URI

	// Accounts returns one entry per configured account.
	Accounts() []Account

	// Handle executes a single JMAP method call.
	// args are fully resolved (result references substituted).
	// Return (result, nil) on success; (nil, err) to send a methodError response.
	// Return errors with message "cannotCalculateChanges" to send that specific error type.
	Handle(method string, args json.RawMessage) (any, error)
}

// Hub broadcasts JMAP state-change events to SSE subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]bool

	// Web Push (see push.go). Populated only if SetVAPIDKeys is called;
	// otherwise Notify's push fan-out is a no-op.
	pushMu          sync.Mutex
	pushSubs        map[jmap.ID][]PushSubscription
	pushDir         string
	vapidPublic     string
	vapidPrivate    string
	vapidSubscriber string
}

// NewHub creates a new Hub.
func NewHub() *Hub {
	return &Hub{subs: map[chan struct{}]bool{}}
}

// Notify sends a state-change event to all current SSE subscribers, and wakes
// any registered Web Push subscriptions (see push.go) so backgrounded/closed
// clients can refresh too.
func (h *Hub) Notify() {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
	go h.pushAll()
}

func (h *Hub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// NewMux returns an http.ServeMux with JMAP endpoints registered.
// Use this when you need to add extra routes (e.g. ActivityPub) to the same
// mux before calling http.ListenAndServe yourself.
// hub may be nil; if non-nil, a /jmap/eventsource/ SSE endpoint is added.
func NewMux(cfg Config, h Handler, hub *Hub) *http.ServeMux {
	s := &srv{cfg: cfg, h: h}
	if hub != nil && cfg.VapidPublicKey != "" {
		hub.SetVAPIDKeys(cfg.VapidPublicKey, cfg.VapidPrivateKey, cfg.VapidSubscriber)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jmap", s.cors(s.auth(s.serveSession)))
	mux.HandleFunc("/jmap/api/", s.cors(s.auth(s.serveAPI)))
	if bh, ok := h.(BlobHandler); ok {
		mux.HandleFunc("/jmap/upload/", s.cors(s.auth(serveBlobUpload(bh))))
		mux.HandleFunc("/jmap/download/", s.cors(s.auth(serveBlobDownload(bh))))
	}
	if hub != nil {
		mux.HandleFunc("/jmap/eventsource/", s.cors(s.auth(func(w http.ResponseWriter, r *http.Request) {
			serveEventSource(w, r, hub)
		})))
		mux.HandleFunc("/jmap/push/vapid-public-key", s.cors(servePushVapidKey(hub)))
		mux.HandleFunc("/jmap/push/subscribe", s.cors(s.auth(servePushSubscribe(hub))))
		mux.HandleFunc("/jmap/push/unsubscribe", s.cors(s.auth(servePushUnsubscribe(hub))))
	}
	return mux
}

func (s *srv) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// Serve starts the JMAP HTTP server and blocks until it returns an error.
// hub may be nil; if non-nil, a /jmap/eventsource/ SSE endpoint is added.
func Serve(cfg Config, h Handler, hub *Hub) error {
	addr := cfg.ListenAddr
	if addr == "" {
		addr = "0.0.0.0:8765"
	}
	return http.ListenAndServe(addr, NewMux(cfg, h, hub))
}

// ── internal ──────────────────────────────────────────────────────────────────

type srv struct {
	cfg Config
	h   Handler
}

func (s *srv) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password := s.extractCredentials(r)
		accountID, ok := s.authenticate(username, password)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="jmap"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxAccountID, accountID)
		next(w, r.WithContext(ctx))
	}
}

func (s *srv) extractCredentials(r *http.Request) (username, password string) {
	// Bearer token: "email:password" or legacy plain password
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		tok := strings.TrimPrefix(a, "Bearer ")
		if parts := strings.SplitN(tok, ":", 2); len(parts) == 2 {
			return parts[0], parts[1]
		}
		return "", tok
	}
	// Basic auth
	if u, p, ok := r.BasicAuth(); ok {
		return u, p
	}
	// access_token query param
	if tok := r.URL.Query().Get("access_token"); tok != "" {
		if parts := strings.SplitN(tok, ":", 2); len(parts) == 2 {
			return parts[0], parts[1]
		}
		return "", tok
	}
	return "", ""
}

func (s *srv) authenticate(username, password string) (jmap.ID, bool) {
	if s.cfg.AuthFunc != nil {
		return s.cfg.AuthFunc(username, password)
	}
	// Legacy: single global password
	if s.cfg.Password != "" {
		if password != s.cfg.Password {
			return "", false
		}
		return jmap.ID(username), true
	}
	// No auth configured
	return jmap.ID(username), true
}

func (s *srv) serveSession(w http.ResponseWriter, r *http.Request) {
	caps := s.h.Capabilities()

	rawCaps := make(map[jmap.URI]json.RawMessage, len(caps)+1)
	rawCaps["urn:ietf:params:jmap:core"] = json.RawMessage(`{` +
		`"maxSizeUpload":50000000,` +
		`"maxConcurrentUpload":4,` +
		`"maxSizeRequest":10000000,` +
		`"maxConcurrentRequests":4,` +
		`"maxCallsInRequest":32,` +
		`"maxObjectsInGet":500,` +
		`"maxObjectsInSet":500,` +
		`"collationAlgorithms":[]` +
		`}`)
	acctCaps := make(map[jmap.URI]json.RawMessage, len(caps))
	for _, uri := range caps {
		rawCaps[uri] = json.RawMessage(`{}`)
		acctCaps[uri] = json.RawMessage(`{}`)
	}

	authedID, _ := r.Context().Value(ctxAccountID).(jmap.ID)

	all := s.h.Accounts()
	jmapAccounts := make(map[jmap.ID]jmap.Account, len(all))
	primaryAccounts := make(map[jmap.URI]jmap.ID, len(caps))
	username := ""

	for _, a := range all {
		if authedID != "" && a.ID != authedID {
			continue
		}
		jmapAccounts[a.ID] = jmap.Account{
			Name:            a.Name,
			IsPersonal:      true,
			IsReadOnly:      false,
			RawCapabilities: acctCaps,
		}
		if username == "" {
			username = a.Name
			for _, uri := range caps {
				primaryAccounts[uri] = a.ID
			}
		}
	}

	base := strings.TrimRight(s.cfg.BaseURL, "/")
	if base == "" {
		addr := s.cfg.ListenAddr
		if addr == "" {
			addr = "0.0.0.0:8765"
		}
		base = "http://" + addr
	}
	sess := jmap.Session{
		RawCapabilities: rawCaps,
		Accounts:        jmapAccounts,
		PrimaryAccounts: primaryAccounts,
		Username:        username,
		APIURL:          base + "/jmap/api/",
		DownloadURL:     base + "/jmap/download/{accountId}/{blobId}/{name}?accept={type}",
		UploadURL:       base + "/jmap/upload/{accountId}/",
		EventSourceURL:  base + "/jmap/eventsource/",
		State:           "0",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sess) //nolint:errcheck
}

func (s *srv) serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		MethodCalls []json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	results := map[string]json.RawMessage{}
	var responses []json.RawMessage

	for _, rawCall := range req.MethodCalls {
		var call [3]json.RawMessage
		if err := json.Unmarshal(rawCall, &call); err != nil {
			continue
		}
		var name, callID string
		json.Unmarshal(call[0], &name)   //nolint:errcheck
		json.Unmarshal(call[2], &callID) //nolint:errcheck

		resolvedArgs, err := resolveRefs(call[1], results)
		if err != nil {
			responses = append(responses, errorResponse(name, callID, "serverFail", err.Error()))
			continue
		}

		result, handleErr := s.h.Handle(name, resolvedArgs)
		if handleErr != nil {
			errType := "serverFail"
			if handleErr.Error() == "cannotCalculateChanges" {
				errType = "cannotCalculateChanges"
			}
			responses = append(responses, errorResponse(name, callID, errType, handleErr.Error()))
			continue
		}

		if b, err := json.Marshal(result); err == nil {
			results[callID] = b
		}
		resultJSON, _ := json.Marshal(result)
		resp, _ := json.Marshal([]json.RawMessage{marshal(name), resultJSON, marshal(callID)})
		responses = append(responses, resp)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"sessionState":    "0",
		"methodResponses": responses,
	})
}

func serveEventSource(w http.ResponseWriter, r *http.Request, hub *Hub) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: state\ndata: {\"changed\":{\"urn:ietf:params:jmap:mail\":null}}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	ch := hub.subscribe()
	defer hub.unsubscribe(ch)
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ch:
			fmt.Fprint(w, "event: state\ndata: {\"changed\":{\"urn:ietf:params:jmap:mail\":null}}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func errorResponse(name, callID, errType, desc string) json.RawMessage {
	r, _ := json.Marshal([]json.RawMessage{
		marshal(name),
		marshal(map[string]string{"type": errType, "description": desc}),
		marshal(callID),
	})
	return r
}

func marshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func serveBlobUpload(bh BlobHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		ct := r.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/octet-stream"
		}
		blobID := bh.UploadBlob(ct, data)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"blobId": blobID,
			"type":   ct,
			"size":   len(data),
		})
	}
}

func serveBlobDownload(bh BlobHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// URL: /jmap/download/{accountId}/{blobId}/{name}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jmap/download/"), "/")
		if len(parts) < 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		blobID := parts[1]
		data, ok := bh.DownloadBlob(blobID)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data) //nolint:errcheck
	}
}

// resolveRefs substitutes result-reference arguments (keys prefixed with "#").
func resolveRefs(args json.RawMessage, results map[string]json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return args, nil
	}
	hasRef := false
	for k := range m {
		if strings.HasPrefix(k, "#") {
			hasRef = true
			break
		}
	}
	if !hasRef {
		return args, nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if !strings.HasPrefix(k, "#") {
			out[k] = v
			continue
		}
		var ref struct {
			ResultOf string `json:"resultOf"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(v, &ref); err != nil {
			return nil, fmt.Errorf("bad result reference %s: %w", k, err)
		}
		prev, ok := results[ref.ResultOf]
		if !ok {
			return nil, fmt.Errorf("no result for callId %q", ref.ResultOf)
		}
		resolved, err := jsonPath(prev, ref.Path)
		if err != nil {
			return nil, fmt.Errorf("path %q in %q: %w", ref.Path, ref.ResultOf, err)
		}
		out[k[1:]] = resolved
	}
	return json.Marshal(out)
}

// jsonPath extracts a value from JSON using a simple slash-delimited path (e.g. "/list/0/id").
func jsonPath(data json.RawMessage, path string) (json.RawMessage, error) {
	var cur any
	if err := json.Unmarshal(data, &cur); err != nil {
		return nil, err
	}
	for _, p := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if p == "" {
			continue
		}
		switch v := cur.(type) {
		case map[string]any:
			var ok bool
			cur, ok = v[p]
			if !ok {
				return nil, fmt.Errorf("key %q not found", p)
			}
		case []any:
			idx, err := strconv.Atoi(p)
			if err != nil {
				return nil, fmt.Errorf("expected array index at %q", p)
			}
			if idx < 0 || idx >= len(v) {
				return nil, fmt.Errorf("index %d out of range (len %d)", idx, len(v))
			}
			cur = v[idx]
		default:
			return nil, fmt.Errorf("expected object or array at %q", p)
		}
	}
	return json.Marshal(cur)
}
