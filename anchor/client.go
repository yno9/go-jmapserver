// Package anchor is a relay's client for the standalone identity anchor
// (biset-anchor): the DID-coordination layer that jmapap and jmapsmtp share.
//
// It lives apart from package jmapserver on purpose. jmapserver is the JMAP
// core; anchoring is orthogonal to it, and a relay built without this package
// (`go build -tags noanchor`) is a pure JMAP server that speaks no DID at all —
// no claim, no release, no /pkarr, no /account/did. Nothing in package
// jmapserver imports this, so that exclusion is a compile-time fact, not a
// runtime flag.
package anchor

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

// Ref is where this relay's anchor lives and how it proves it is allowed to
// write there.
//
// **The token is not optional.** The anchor is on the public internet — it has
// to be, because clients reach its DIDComm mediator directly — and its registry
// decides who owns which address. Without a shared secret anyone who can reach
// it can DELETE the claim of somebody who has one and take it, DNS record and
// all — a release names no key and proves nothing, so "can reach the anchor"
// was the entire authorization story for it.
//
// This is the piece ANCHOR.md's threat model missed. It argued the anchor may
// trust a relay's word about r.Host because "a lying relay could already claim
// anything it liked on the anchor" — true, but the premise underneath it was
// that only relays talk to the anchor. Nothing enforced that. Now something
// does.
type Ref struct {
	URL   string // empty = anchorless; this relay serves no DID identities
	Token string // shared with the anchor's relay_token
}

func (a Ref) request(method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, strings.TrimRight(a.URL, "/")+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	return req, nil
}

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

// Claim asks the identity anchor service to record which DID owns
// localpart+domain — the shared HTTP client both jmapap and jmapsmtp use, so
// relays that want DID identity coordination don't each reimplement this.
//
// domain is the real address domain (e.g. t.biset.md) — distinct from the
// anchor's own host, since one anchor instance serves every domain a relay
// family provisions under.
//
// A DID and its proof are the only things there is to claim by, and both are
// mandatory. This used to take an envelope fingerprint too, for accounts with no
// DID — nothing read it, and an account with no DID publishes nothing a claim
// could protect. Its removal took the proof-less claim with it: every caller now
// has a signature, the anchor 401s a DID without one, so an optional proof could
// only ever have meant "fail".
//
// Returns "ok" (claim recorded or matched), "conflict" (the name is held by a
// different DID), "invalid" (the anchor rejected the binding proof — bad
// signature, wrong host, or stale timestamp), or "error" (anchor unreachable,
// refusing this relay, or a bad response).
func Claim(a Ref, localpart, domain, did string, proof BindingProof) string {
	body, _ := json.Marshal(map[string]any{
		"domain":  domain,
		"did":     did,
		"did_sig": proof.Sig,
		"bind_ts": proof.TS,
		"host":    proof.Host,
	})
	req, err := a.request(http.MethodPost, "/identity/"+localpart, body)
	if err != nil {
		return "error"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
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
	case http.StatusForbidden:
		// Not the client's fault and not something they can fix: this relay is
		// the one being turned away. Distinct from 401 precisely so it cannot be
		// reported to a user as "your DID proof was rejected" — the proof was
		// never looked at. Provisioning treats it like any other anchor failure
		// and refuses rather than proceeding unanchored.
		log.Printf("[anchor] REFUSED THIS RELAY (%s) — check anchor_token against the anchor's relay_token", a.URL)
		return "error"
	default:
		return "error"
	}
}

// Release tells the anchor to forget localpart+domain's claim — call when an
// account is permanently deleted, so the address becomes claimable again.
// Without this, a legitimate future registration of the same address (by
// anyone, including its original owner under a new identity) would be rejected
// by Claim as a false "different key" conflict — the deleted account's stale
// claim would never go away otherwise. Best-effort like every other anchor call
// here: an unreachable anchor must never block the surrounding account-delete
// flow, so the outcome is discarded.
func Release(a Ref, localpart, domain string) {
	releaseOK(a, localpart, domain)
}

// releaseOK performs the DELETE and reports whether the anchor confirmed it.
// Release throws the result away — an unreachable anchor must not block an
// account deletion — but Drain needs it: a release it could not confirm means
// the claim may still stand, and a relay must not believe it has gone anchorless
// cleanly when it hasn't. Release is idempotent at the anchor (deleting an
// address that holds no claim is a 2xx no-op), so this says "clear at the
// anchor", not "there was a claim and we removed it".
func releaseOK(a Ref, localpart, domain string) bool {
	if a.URL == "" {
		return false
	}
	req, err := a.request(http.MethodDelete, "/identity/"+localpart+"?domain="+url.QueryEscape(domain), nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		log.Printf("[anchor] REFUSED THIS RELAY (%s) on release of %s@%s — check anchor_token", a.URL, localpart, domain)
		return false
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
