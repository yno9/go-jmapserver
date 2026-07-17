package jmapserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The anchor parses these exact keys (biset src/anchor/server.ts) and verifies a
// signature over a statement built from them. Renaming one, or letting bind_ts
// go out as a string, fails no build and no other test — it just makes every DID
// account creation 401 in production. So the wire format is pinned here.
func TestAnchorClaimForwardsBindingProof(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/identity/alice" {
			t.Errorf("path = %q, want /identity/alice", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	proof := &BindingProof{Sig: "c2ln", TS: 1752700000, Host: "mail.biset.md"}
	if r := AnchorClaim(AnchorRef{URL: srv.URL, Token: "t"}, "alice", "biset.md", "", "did:dht:abc", proof); r != "ok" {
		t.Fatalf("AnchorClaim = %q, want ok", r)
	}
	for k, want := range map[string]any{
		"domain":  "biset.md",
		"did":     "did:dht:abc",
		"did_sig": "c2ln",
		"host":    "mail.biset.md",
		"bind_ts": float64(1752700000), // a JSON number, not a string
	} {
		if got[k] != want {
			t.Errorf("body[%q] = %#v, want %#v", k, got[k], want)
		}
	}
}

// A nil proof must leave the proof keys off entirely rather than send empty
// ones: the anchor treats "did_sig present" as "a proof was offered, so it must
// verify". An empty string would turn every proof-less claim — backfill,
// envelope rotation, lazy DID migration — into a 401.
func TestAnchorClaimWithoutProofOmitsProofKeys(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if r := AnchorClaim(AnchorRef{URL: srv.URL, Token: "t"}, "alice", "biset.md", "fp", "", nil); r != "ok" {
		t.Fatalf("AnchorClaim = %q, want ok", r)
	}
	for _, k := range []string{"did_sig", "bind_ts", "host"} {
		if _, present := got[k]; present {
			t.Errorf("body[%q] present for a proof-less claim: %#v", k, got[k])
		}
	}
	if got["fingerprint"] != "fp" {
		t.Errorf("fingerprint = %#v, want fp", got["fingerprint"])
	}
}

// The four outcomes are acted on differently by both relays: "invalid" is the
// client's fault (401), "conflict" is someone else's name (409), "error" is the
// anchor's fault (503, and provisioning refuses rather than proceeds unanchored).
// Collapsing any pair would either leak a name or accept an unproven DID.
func TestAnchorClaimStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusOK, "ok"},
		{http.StatusCreated, "ok"},
		{http.StatusConflict, "conflict"},
		{http.StatusUnauthorized, "invalid"},
		{http.StatusServiceUnavailable, "error"},
		{http.StatusForbidden, "error"}, // this relay turned away — never reported as a client-side proof failure
		{http.StatusInternalServerError, "error"},
		{http.StatusBadRequest, "error"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		if r := AnchorClaim(AnchorRef{URL: srv.URL, Token: "t"}, "alice", "biset.md", "", "did:dht:abc", nil); r != tc.want {
			t.Errorf("status %d -> %q, want %q", tc.status, r, tc.want)
		}
		srv.Close()
	}
}

// Every write carries the relay's token. Without it the anchor's registry is
// writable by anyone who can reach it — and it is reachable by everyone, since
// its DIDComm mediator has to be.
func TestAnchorWritesCarryTheRelayToken(t *testing.T) {
	var claimAuth, releaseAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			releaseAuth = r.Header.Get("Authorization")
		} else {
			claimAuth = r.Header.Get("Authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ref := AnchorRef{URL: srv.URL, Token: "s3cr3t"}
	AnchorClaim(ref, "alice", "biset.md", "fp", "", nil)
	AnchorRelease(ref, "alice", "biset.md")

	if claimAuth != "Bearer s3cr3t" {
		t.Errorf("claim Authorization = %q, want %q", claimAuth, "Bearer s3cr3t")
	}
	if releaseAuth != "Bearer s3cr3t" {
		t.Errorf("release Authorization = %q, want %q — an unauthenticated DELETE is how a claim gets taken from its owner", releaseAuth, "Bearer s3cr3t")
	}
}

// An unreachable anchor must be distinguishable from every rejection, because
// provisioning refuses on it rather than treating it as a verdict.
func TestAnchorClaimUnreachable(t *testing.T) {
	if r := AnchorClaim(AnchorRef{URL: "http://127.0.0.1:1", Token: "t"}, "alice", "biset.md", "", "did:dht:abc", nil); r != "error" {
		t.Fatalf("AnchorClaim = %q, want error", r)
	}
}
