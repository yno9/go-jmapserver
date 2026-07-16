package jmapserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Local DID→addresses index (DID.md, 2026-07-13): a purely organizational
// index — "which addresses on THIS relay belong to this DID" — kept
// separate from the cross-relay anchor (anchor answers "is this DID's name
// globally unique"; this answers "what does this relay itself know about
// it"). Every protocol-account keeps its own independent Store; this does
// NOT merge storage.
//
// An earlier version of this file made a second address for an existing DID
// share the FIRST address's literal Store instance (a hard-link-like alias).
// That was walked back: the concrete benefit (a unified inbox view) was
// already delivered by the client's existing identity-by-DID grouping
// (did || email — see did/discovery.ts and context.ts's identityKey), so
// physically merging storage bought little while introducing a real bug
// (two sessions racing to sync the identical mailbox, see DID.md's
// data-model-inversion section) and requiring the client to learn a new
// "is this an alias" concept it otherwise didn't need. Being DID-rooted in
// *organization* (every address traceable back to its owning DID) and being
// DID-merged in *storage* turned out to be separable — only the former was
// ever actually load-bearing.
//
// Shared by every relay type (jmapap, jmapsmtp, future siblings) — "share
// libraries, not state" — same as the rest of the DID layer.

func didLocalIndexDir(dataDir string) string { return filepath.Join(dataDir, "_did_local") }

func didLocalIndexPath(dataDir, did string) string {
	return filepath.Join(didLocalIndexDir(dataDir), did)
}

// LookupLocalDID returns every address this DID has an independent store
// under on this relay (nil if none).
func LookupLocalDID(dataDir, did string) []string {
	b, err := os.ReadFile(didLocalIndexPath(dataDir, did))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// RecordLocalDID appends email to this DID's local address list — call once
// per new account creation carrying a DID. Idempotent.
func RecordLocalDID(dataDir, did, email string) {
	for _, e := range LookupLocalDID(dataDir, did) {
		if e == email {
			return // already recorded
		}
	}
	path := didLocalIndexPath(dataDir, did)
	os.MkdirAll(filepath.Dir(path), 0700) //nolint:errcheck
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(email + "\n") //nolint:errcheck
}

// RemoveLocalDID drops email from this DID's local address list — call when
// an account is deleted. Best-effort, same safety level as RecordLocalDID
// (no cross-process lock exists in this file; see the package comment
// above) — a delete racing a concurrent record isn't newly introduced by
// this function, just not newly solved by it either. No-op if the DID has
// no local index file, or email isn't in it. Removes the file entirely if
// this was the last remaining address.
func RemoveLocalDID(dataDir, did, email string) error {
	addrs := LookupLocalDID(dataDir, did)
	if addrs == nil {
		return nil
	}
	out := addrs[:0]
	for _, e := range addrs {
		if e != email {
			out = append(out, e)
		}
	}
	path := didLocalIndexPath(dataDir, did)
	if len(out) == 0 {
		return os.Remove(path)
	}
	var b strings.Builder
	for _, e := range out {
		b.WriteString(e)
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0600)
}

// RegisterDIDLocalIndex exposes the local index for operational lookups
// ("what accounts does this DID have on this relay" — a question that came
// up repeatedly during development, done by hand via SSH each time). Public
// and unauthenticated: it discloses nothing more than the DID/DNS layer
// already publishes by design (an address is meant to be discoverable given
// its DID) — same sensitivity level as the anchor's own by-did lookup.
//
//	GET /identity/local/<did>  → {"addresses": ["y@biset.md", ...]}
func RegisterDIDLocalIndex(mux *http.ServeMux, dataDir string) {
	mux.HandleFunc("/identity/local/", func(w http.ResponseWriter, r *http.Request) {
		did := strings.TrimPrefix(r.URL.Path, "/identity/local/")
		if did == "" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		addrs := LookupLocalDID(dataDir, did)
		if addrs == nil {
			addrs = []string{}
		}
		json.NewEncoder(w).Encode(map[string][]string{"addresses": addrs}) //nolint:errcheck
	})
}
