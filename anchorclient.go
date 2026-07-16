package jmapserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
// Returns "ok" (claim recorded or matched), "conflict" (name held by a
// different key, or a DID mismatch), or "error" (anchor unreachable/bad
// response).
func AnchorClaim(anchorURL, localpart, domain, fp, did string) string {
	body, _ := json.Marshal(map[string]string{"domain": domain, "fingerprint": fp, "did": did})
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
