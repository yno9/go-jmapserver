package anchor

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// RegisterPkarrProxy forwards /pkarr/* to the anchor, which runs the actual
// Mainline DHT node (ANCHOR.md decision 1). Every relay used to run its own —
// its own UDP socket, its own routing table, its own republish loop, all of it
// duplicated per relay and none of it the relay's job.
//
// The route stays because the CLIENT'S route stays: biset derives its gateway
// URL from the account's own relay (`serverUrl + "/pkarr"`, see src/did/
// publish.ts), and publishing goes to those relays ONLY — the public fallbacks
// are used for resolving, never for publishing. Removing this endpoint would
// strand every already-loaded client: it would have nowhere to publish, this
// relay would no longer republish, and those identities would fade off the DHT
// within hours. So the relay keeps answering and passes the request along.
//
// It also keeps the privacy story intact (src/did/resolver.ts: "resolving
// through a stranger's relay leaks who-looks-up-whom"). The client still asks
// its own relay; the relay asks the anchor; both belong to the same operator,
// which is the only reason the anchor may see lookups at all.
//
// Nothing here understands DIDs: the key is an opaque path segment and the body
// an opaque blob. Validation, signature checking and the DHT all live at the
// far end.
func RegisterPkarrProxy(mux *http.ServeMux, a Ref) {
	if a.URL == "" {
		return // anchorless: no DHT gateway to reach (ANCHOR.md decision 2)
	}
	base := strings.TrimRight(a.URL, "/") + "/pkarr/"
	// Generous next to the anchor's own 30s DHT timeout: a traversal that is
	// still going is worth waiting for, and the client already treats a failure
	// as "try the next gateway".
	client := &http.Client{Timeout: 40 * time.Second}

	mux.HandleFunc("/pkarr/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/pkarr/")
		if key == "" || strings.Contains(key, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, base+key, r.Body)
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		// The anchor's /pkarr is for its own relays, not the world: this route is
		// the public face, and forwarding without the token would leave the anchor
		// a gateway anyone could spend — and, per resolver.ts's privacy note, one
		// that sees strangers' lookups too.
		req.Header.Set("Authorization", "Bearer "+a.Token)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "pkarr gateway unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
	})
}
