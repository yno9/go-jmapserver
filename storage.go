package jmapserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// "How your data is stored" transparency feature (biset issue #7): let an
// account holder see the real on-disk shape of their own data, export it
// whole, and clear just the message data if they want a clean slate without
// deleting the account itself (that's /account/delete).

// StorageEntry describes one top-level entry in an account's data directory.
type StorageEntry struct {
	Name      string `json:"name"`
	Type      string `json:"type"`            // "file" or "dir"
	Count     int    `json:"count,omitempty"` // dir only: files inside
	SizeBytes int64  `json:"sizeBytes"`
}

// listAccountStorage walks the account directory ONE level deep. `messages/`
// (one JSON file per message — sometimes thousands) is summarized as a
// single dir entry with a count + total size rather than listed file-by-
// file: a per-message tree would be unusably long and isn't what "how your
// data is stored" is asking to see. Any other subdirectory is skipped
// defensively (none are expected in the current layout).
func listAccountStorage(dataDir, domain, localpart string) ([]StorageEntry, error) {
	acctDir := filepath.Join(dataDir, domain, localpart)
	entries, err := os.ReadDir(acctDir)
	if err != nil {
		return nil, err
	}
	out := make([]StorageEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			if e.Name() != "messages" {
				continue
			}
			count, size := 0, int64(0)
			msgEntries, _ := os.ReadDir(filepath.Join(acctDir, "messages"))
			for _, me := range msgEntries {
				if me.IsDir() {
					continue
				}
				if info, err := me.Info(); err == nil {
					count++
					size += info.Size()
				}
			}
			out = append(out, StorageEntry{Name: "messages", Type: "dir", Count: count, SizeBytes: size})
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, StorageEntry{Name: e.Name(), Type: "file", SizeBytes: info.Size()})
	}
	return out, nil
}

// listMessageFiles lists every individual file under messages/ — the drill-
// down behind clicking the summarized "messages" entry from
// listAccountStorage. Separate endpoint rather than folding into the
// one-level listing above, since callers who only want the top-level
// summary (the common case) shouldn't pay for a directory read that could
// return thousands of entries.
func listMessageFiles(dataDir, domain, localpart string) ([]StorageEntry, error) {
	dir := filepath.Join(dataDir, domain, localpart, "messages")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]StorageEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, StorageEntry{Name: e.Name(), Type: "file", SizeBytes: info.Size()})
	}
	return out, nil
}

// exportAccountStorage reads every file under the account directory
// (including inside messages/) and returns relative-path → raw bytes, for
// the caller to encode. "How your data is stored", literally: every file
// exactly as it sits on disk, nothing synthesized or filtered.
func exportAccountStorage(dataDir, domain, localpart string) (map[string][]byte, error) {
	acctDir := filepath.Join(dataDir, domain, localpart)
	out := map[string][]byte{}
	err := filepath.Walk(acctDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr
		}
		rel, relErr := filepath.Rel(acctDir, path)
		if relErr != nil {
			return nil //nolint:nilerr
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	return out, err
}

// RegisterStorageEndpoints exposes read/export/purge over the account's own
// on-disk data. Same authenticated-by-credential-only shape as every other
// per-account endpoint in this package family: no email in any request, the
// target is derived purely from the Basic Auth credential, so this can never
// act on anyone else's account.
//
//	GET  /account/storage                → {"entries":[...],"totalSizeBytes":N}
//	GET  /account/storage/messages        → {"files":[{"name","type","sizeBytes"},...]} (drill-down into the "messages" entry above)
//	GET  /account/storage/export          → {"email":"...","files":{"path":"base64",...}}
//	POST /account/storage/purge-messages  → {"purged":N}
//
// purge-messages touches ONLY messages/ (via purgeMessages, a per-relay
// closure over h.stores[email].Purge() + hub.Notify() — the in-memory Store
// instance and its live subscribers are relay-specific state this package
// doesn't own) — it never touches mailboxes.json/identities.json/
// contacts.json/envelope.json/auth_token_hash. Deleting those would corrupt
// or lock the account out entirely; that's what full account deletion
// (/account/delete) is for, not this.
func RegisterStorageEndpoints(mux *http.ServeMux, dataDir string, authenticate func(r *http.Request, dataDir string) (domain, localpart string, ok bool), purgeMessages func(email string) int) {
	cors := func(w http.ResponseWriter, methods string) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", methods)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	}
	auth := func(w http.ResponseWriter, r *http.Request) (domain, localpart string, ok bool) {
		domain, localpart, ok = authenticate(r, dataDir)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="biset"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
		return
	}

	mux.HandleFunc("/account/storage", func(w http.ResponseWriter, r *http.Request) {
		cors(w, "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		domain, localpart, ok := auth(w, r)
		if !ok {
			return
		}
		entries, err := listAccountStorage(dataDir, domain, localpart)
		if err != nil {
			http.Error(w, "failed to read storage", http.StatusInternalServerError)
			return
		}
		var total int64
		for _, e := range entries {
			total += e.SizeBytes
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"entries": entries, "totalSizeBytes": total}) //nolint:errcheck
	})

	mux.HandleFunc("/account/storage/messages", func(w http.ResponseWriter, r *http.Request) {
		cors(w, "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		domain, localpart, ok := auth(w, r)
		if !ok {
			return
		}
		files, err := listMessageFiles(dataDir, domain, localpart)
		if err != nil {
			http.Error(w, "failed to read messages", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"files": files}) //nolint:errcheck
	})

	mux.HandleFunc("/account/storage/export", func(w http.ResponseWriter, r *http.Request) {
		cors(w, "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		domain, localpart, ok := auth(w, r)
		if !ok {
			return
		}
		files, err := exportAccountStorage(dataDir, domain, localpart)
		if err != nil {
			http.Error(w, "failed to export storage", http.StatusInternalServerError)
			return
		}
		encoded := make(map[string]string, len(files))
		for path, b := range files {
			encoded[path] = base64.StdEncoding.EncodeToString(b)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"email": localpart + "@" + domain,
			"files": encoded,
		})
	})

	mux.HandleFunc("/account/storage/purge-messages", func(w http.ResponseWriter, r *http.Request) {
		cors(w, "POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		domain, localpart, ok := auth(w, r)
		if !ok {
			return
		}
		n := purgeMessages(localpart + "@" + domain)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"purged": n}) //nolint:errcheck
	})
}
