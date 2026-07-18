package anchor

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A clean drain: every name released, Failed empty. The anchor confirms each
// with a 2xx (204 in practice), and Drain must send a DELETE carrying the relay
// token — an unauthenticated release is how a claim gets taken from its owner.
func TestDrainReleasesAll(t *testing.T) {
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", r.Header.Get("Authorization"))
		}
		deletes++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	names := []Name{
		{Localpart: "alice", Domain: "biset.md"},
		{Localpart: "bob", Domain: "biset.md"},
		{Localpart: "carol", Domain: "t.biset.md"},
	}
	rep := Drain(Ref{URL: srv.URL, Token: "tok"}, names)
	if len(rep.Released) != 3 || len(rep.Failed) != 0 {
		t.Fatalf("released=%d failed=%d, want 3/0", len(rep.Released), len(rep.Failed))
	}
	if deletes != 3 {
		t.Errorf("anchor saw %d DELETEs, want 3", deletes)
	}
}

// The safety property: an anchor Drain could not reach reports every name as
// Failed, NEVER Released. A relay that believed an unreachable drain was clean
// would go anchorless leaving live claims behind — stranding those names for
// every other relay and for their owners.
func TestDrainUnreachableFailsNeverReleases(t *testing.T) {
	rep := Drain(Ref{URL: "http://127.0.0.1:1", Token: "tok"}, []Name{
		{Localpart: "alice", Domain: "biset.md"},
		{Localpart: "bob", Domain: "biset.md"},
	})
	if len(rep.Released) != 0 {
		t.Fatalf("released=%d, want 0 — an unreachable anchor released nothing", len(rep.Released))
	}
	if len(rep.Failed) != 2 {
		t.Fatalf("failed=%d, want 2", len(rep.Failed))
	}
}

// A relay the anchor turns away (403) is a Failed drain, not a silent success:
// the claims are untouched, so reporting them released would be a lie that ends
// in stranded names.
func TestDrainForbiddenIsFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	rep := Drain(Ref{URL: srv.URL, Token: "wrong"}, []Name{{Localpart: "alice", Domain: "biset.md"}})
	if len(rep.Failed) != 1 || len(rep.Released) != 0 {
		t.Fatalf("released=%d failed=%d, want 0/1", len(rep.Released), len(rep.Failed))
	}
}

// Mixed outcomes are partitioned per name, not collapsed to all-or-nothing: the
// operator needs the exact Failed list to know which claims may still stand.
func TestDrainPartitionsMixed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "gone" releases cleanly; everyone else is refused.
		if r.URL.Path == "/identity/gone" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	rep := Drain(Ref{URL: srv.URL, Token: "tok"}, []Name{
		{Localpart: "gone", Domain: "biset.md"},
		{Localpart: "stuck", Domain: "biset.md"},
	})
	if len(rep.Released) != 1 || rep.Released[0].Localpart != "gone" {
		t.Errorf("released = %+v, want [gone]", rep.Released)
	}
	if len(rep.Failed) != 1 || rep.Failed[0].Localpart != "stuck" {
		t.Errorf("failed = %+v, want [stuck]", rep.Failed)
	}
}
