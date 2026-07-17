package jmapserver

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DID-rooted contact cache (DID.md): a JSContact (RFC 9553) Card per resolved
// contact, one relay-account-scoped file. This is the server-side half of the
// write-through cache client discovery.ts already keeps in localStorage — it
// exists so a contact binding survives a device change or a browser that
// doesn't support the vault (File System Access API is Chromium-only), by
// letting the client pull its own history back from its own relay.
//
// Fields are mapped onto native JSContact properties wherever one exists
// (a DID is itself a URI, so it fits `cryptoKeys` without any extension);
// only biset-specific bookkeeping uses the vendor-extension form the spec
// requires (`biset.md:<name>`).

// EmailAddr is a JSContact EmailAddress value (the address property is
// mandatory; everything else is optional and omitted here).
type EmailAddr struct {
	Address string `json:"address"`
}

// CryptoKey is a JSContact CryptoKey resource. uri is mandatory and, for our
// use, is always a `did:...` string — DIDs are URIs by construction, so no
// extension property is needed to carry them.
type CryptoKey struct {
	URI string `json:"uri"`
}

// Link is a JSContact Link resource — used here for the contact's current
// relay/service endpoints (as published in their DID document).
type Link struct {
	URI string `json:"uri"`
}

// Card is a JSContact Card, restricted to the properties biset populates.
type Card struct {
	Type       string               `json:"@type"`
	Version    string               `json:"version"`
	UID        string               `json:"uid"`
	Emails     map[string]EmailAddr `json:"emails,omitempty"`
	CryptoKeys map[string]CryptoKey `json:"cryptoKeys,omitempty"`
	Links      map[string]Link      `json:"links,omitempty"`
	VerifiedAt int64                `json:"biset.md:verifiedAt,omitempty"`
}

// Cards live at <accountDir>/contacts.json — accountDir is the same
// per-account directory the relay already scopes everything else to
// (dataDir/domain/localpart), passed explicitly rather than via a *Store so
// the HTTP layer (which only knows domain/localpart, per relay auth) doesn't
// need a Store lookup callback plumbed through.

func contactsPath(accountDir string) string { return filepath.Join(accountDir, "contacts.json") }

// ReadContacts returns every Card persisted for this account (nil if none yet).
func ReadContacts(accountDir string) []Card {
	b, err := os.ReadFile(contactsPath(accountDir))
	if err != nil {
		return nil
	}
	var cards []Card
	if err := json.Unmarshal(b, &cards); err != nil {
		return nil
	}
	return cards
}

// PutContact upserts a single Card by uid. Cards arrive one at a time (the
// client write-throughs each freshly-resolved contact as it learns it), so
// this merges into the existing list rather than replacing it wholesale.
func PutContact(accountDir string, c Card) error {
	cards := ReadContacts(accountDir)
	for i, existing := range cards {
		if existing.UID == c.UID {
			cards[i] = c
			b, err := json.Marshal(cards)
			if err != nil {
				return err
			}
			return os.WriteFile(contactsPath(accountDir), b, 0644)
		}
	}
	cards = append(cards, c)
	b, err := json.Marshal(cards)
	if err != nil {
		return err
	}
	return os.WriteFile(contactsPath(accountDir), b, 0644)
}

// RegisterContactsEndpoints wires the account's contact-cache sync routes.
// authenticate mirrors each relay's own `authenticate(r, dataDir) (domain,
// localpart string, ok bool)` — passed in rather than assumed, since jmapap
// and jmapsmtp each resolve accounts against their own domain/routing rules.
//
//	GET  /contacts        → {"cards": [...]}   (fresh device/browser restore)
//	PUT  /contacts/<uid>   → upsert one Card    (write-through on each resolve)
func RegisterContactsEndpoints(mux *http.ServeMux, dataDir string, authenticate func(r *http.Request, dataDir string) (domain, localpart string, ok bool)) {
	mux.HandleFunc("/contacts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		domain, localpart, ok := authenticate(r, dataDir)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		accountDir := filepath.Join(dataDir, domain, localpart)
		cards := ReadContacts(accountDir)
		if cards == nil {
			cards = []Card{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string][]Card{"cards": cards}) //nolint:errcheck
	})

	mux.HandleFunc("/contacts/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		uid := strings.TrimPrefix(r.URL.Path, "/contacts/")
		if uid == "" {
			http.NotFound(w, r)
			return
		}
		domain, localpart, ok := authenticate(r, dataDir)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		var card Card
		if err := json.Unmarshal(body, &card); err != nil || card.UID == "" {
			http.Error(w, "invalid card", http.StatusBadRequest)
			return
		}
		if card.UID != uid {
			http.Error(w, "uid mismatch", http.StatusBadRequest)
			return
		}
		accountDir := filepath.Join(dataDir, domain, localpart)
		if err := os.MkdirAll(accountDir, 0700); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := PutContact(accountDir, card); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
