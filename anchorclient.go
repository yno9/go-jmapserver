package jmapserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BindingProof is a client's root-key signature over the host-bound statement
//
//	bind:<did>:<username>@<host>:<ts>
//
// which proves control of the DID being claimed. The relay forwards it to the
// anchor rather than checking it: verification is the anchor's job (ANCHOR.md
// decision 1), so the DID crypto lives in one place instead of in every relay.
//
// Host is the host the client signed against, as this relay observed it on the
// transport (r.Host) — pass it VERBATIM. It is first-hand knowledge the anchor
// does not have, and it is what stops a signature captured on one relay being
// replayed against another.
type BindingProof struct {
	Sig  string // base64, standard alphabet
	TS   int64  // unix seconds; the anchor enforces the freshness window
	Host string // r.Host, verbatim
}

// AnchorClaim asks the (optional, standalone) identity anchor service to
// claim/verify localpart+domain for the given fingerprint and/or DID — the
// shared HTTP client both jmapap and jmapsmtp use, so relays that want DID
// identity coordination don't each reimplement this. A relay that never sets
// an anchor URL simply never calls this (see DID.md "DID is optional" /
// "no-core").
//
// domain is the real address domain (e.g. t.biset.md) — distinct from
// anchorURL's own host, since one anchor instance serves every domain a relay
// family provisions under.
//
// proof is nil for claims that carry no signature: a fingerprint-only claim
// (backfill, envelope rotation) has no DID to prove, and lazy DID migration
// (PUT /account/did) authenticates with the account's own credential instead.
// Only account provisioning has a fresh signature to forward.
//
// Returns "ok" (claim recorded or matched), "conflict" (name held by a
// different key, or a DID mismatch), "invalid" (the anchor rejected the
// binding proof — bad signature, wrong host, or stale timestamp), or "error"
// (anchor unreachable/bad response).
func AnchorClaim(anchorURL, localpart, domain, fp, did string, proof *BindingProof) string {
	payload := map[string]any{"domain": domain, "fingerprint": fp, "did": did}
	if proof != nil {
		payload["did_sig"] = proof.Sig
		payload["bind_ts"] = proof.TS
		payload["host"] = proof.Host
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(anchorURL, "/") + "/identity/" + localpart
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "error"
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return "ok"
	case http.StatusConflict:
		return "conflict"
	case http.StatusUnauthorized:
		// The relay answers the client with a bare 401 — why a proof failed is
		// not the client's business — but the reason must survive somewhere or
		// the likeliest honest failure, a skewed clock, becomes undiagnosable.
		// The anchor states it in the body and nowhere else.
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("[anchor] rejected binding for %s@%s: %s", localpart, domain, strings.TrimSpace(string(reason)))
		return "invalid"
	default:
		return "error"
	}
}

// AnchorRelease tells the anchor to forget localpart+domain's claim — call
// when an account is permanently deleted, so the address becomes claimable
// again. Without this, a legitimate future registration of the same address
// (by anyone, including its original owner under a new identity) would be
// rejected by AnchorClaim as a false "different key" conflict — the deleted
// account's stale claim would never go away otherwise. Best-effort like every
// other anchor call here: an unreachable anchor must never block the
// surrounding account-delete flow, so errors are swallowed.
func AnchorRelease(anchorURL, localpart, domain string) {
	if anchorURL == "" {
		return
	}
	reqURL := strings.TrimRight(anchorURL, "/") + "/identity/" + localpart + "?domain=" + url.QueryEscape(domain)
	req, err := http.NewRequest(http.MethodDelete, reqURL, nil)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
