package pkarr

import (
	"io"
	"net/http"
	"strings"
)

const pubkeyLen = 32

// RegisterGateway mounts the Pkarr relay HTTP surface at /pkarr/:
//
//	GET  /pkarr/<z-base-32 pubkey> → wire payload (sig ‖ seq ‖ v), or 404
//	PUT  /pkarr/<z-base-32 pubkey>   body = wire payload → 204, or 400
//
// biset clients use their own account's relays as gateways (DID.md), so the base
// URL a client is given is e.g. https://mail.non.md/pkarr and it appends the
// z-base-32 key. Records are self-signed: PUT verifies the signature against the
// key in the path, so a gateway can neither forge nor accept a mismatched key.
func RegisterGateway(mux *http.ServeMux, g *Gateway) {
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
		pkBytes, err := zbase32Decode(key, pubkeyLen)
		if err != nil {
			http.Error(w, "invalid key", http.StatusBadRequest)
			return
		}
		var pubkey [32]byte
		copy(pubkey[:], pkBytes)

		switch r.Method {
		case http.MethodGet:
			payload, err := g.Get(r.Context(), pubkey)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			if payload == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(payload) //nolint:errcheck

		case http.MethodPut:
			body, err := io.ReadAll(io.LimitReader(r.Body, headerLen+maxPacketLen+16))
			if err != nil {
				http.Error(w, "read error", http.StatusBadRequest)
				return
			}
			if err := g.Put(r.Context(), pubkey, body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
