package jmapserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DNS-based identity anchor (biset DID.md "biset verse"): the address→DID
// binding lives in a `_did.<localpart>.<domain>` TXT record (`did=<did>`)
// instead of a bespoke relay-hosted endpoint, so discovering an address's DID
// no longer depends on that address's relay operator staying up or honest —
// DNS is a commodity, swappable, and self-hostable by whoever owns the domain.
//
// CloudflareAnchor writes that record via the Cloudflare API. It is the ONLY
// place a Cloudflare credential is held (jmapap only; jmapsmtp forwards
// through the existing anchor HTTP protocol, same as it already does for the
// fingerprint/DID claim itself) — narrowing the blast radius of the token to
// one component, and keeping "who can write DNS" separate from "who runs the
// mail/AP data path" even when, as today, the same admin operates both.
type CloudflareAnchor struct {
	APIToken string
	ZoneID   string
}

func (c CloudflareAnchor) enabled() bool { return c.APIToken != "" && c.ZoneID != "" }

type cfDNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

type cfListResponse struct {
	Success bool          `json:"success"`
	Result  []cfDNSRecord `json:"result"`
}

type cfWriteResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

var cfHTTPClient = &http.Client{Timeout: 10 * time.Second}

func (c CloudflareAnchor) request(method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.cloudflare.com/client/v4"+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")
	return cfHTTPClient.Do(req)
}

// WriteAnchorTXT upserts the `_did.<localpart>.<domain>` TXT record. Rotation-
// less DIDs mean this is write-once in the common case; the update path only
// fires if the record already exists with different content (unexpected, but
// handled rather than left to silently diverge). No-ops (returns nil) when
// Cloudflare isn't configured, so this is safe to call unconditionally.
func (c CloudflareAnchor) WriteAnchorTXT(localpart, domain, did string) error {
	if !c.enabled() {
		return nil
	}
	name := fmt.Sprintf("_did.%s.%s", localpart, domain)
	content := "did=" + did

	listResp, err := c.request("GET", fmt.Sprintf("/zones/%s/dns_records?type=TXT&name=%s", c.ZoneID, url.QueryEscape(name)), nil)
	if err != nil {
		return fmt.Errorf("cloudflare list: %w", err)
	}
	defer listResp.Body.Close()
	var list cfListResponse
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil || !list.Success {
		return fmt.Errorf("cloudflare list: bad response (status %d)", listResp.StatusCode)
	}

	rec := cfDNSRecord{Type: "TXT", Name: name, Content: content, TTL: 300}
	var writeResp *http.Response
	if len(list.Result) == 0 {
		writeResp, err = c.request("POST", fmt.Sprintf("/zones/%s/dns_records", c.ZoneID), rec)
	} else if list.Result[0].Content == content {
		return nil // already correct, nothing to do
	} else {
		writeResp, err = c.request("PATCH", fmt.Sprintf("/zones/%s/dns_records/%s", c.ZoneID, list.Result[0].ID), rec)
	}
	if err != nil {
		return fmt.Errorf("cloudflare write: %w", err)
	}
	defer writeResp.Body.Close()
	var wr cfWriteResponse
	if err := json.NewDecoder(writeResp.Body).Decode(&wr); err != nil || !wr.Success {
		msg := ""
		if len(wr.Errors) > 0 {
			msg = wr.Errors[0].Message
		}
		return fmt.Errorf("cloudflare write failed (status %d): %s", writeResp.StatusCode, msg)
	}
	return nil
}
