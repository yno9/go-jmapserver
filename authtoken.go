package jmapserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// The per-account relay credential.
//
// This file was the relay's DID machinery: it verified DID→relay binding
// signatures and decoded the ed25519 key a did:dht identifier names. Both are
// gone (ANCHOR.md decision 1) — the anchor verifies bindings, and the pkarr
// gateway that needed the decoded key to forget a deleted identity's record now
// lives at the anchor too, which learns of the deletion from the claim it
// releases and never has to be told the DID. **No relay handles DID material
// any more.**
//
// The auth token stayed all along: it only ever shared a file with that code,
// never a purpose.

// HashAuthToken is the per-account credential the relay stores (base64 of the
// SHA-256 of the relay-scoped auth token). VerifyAuthToken compares in constant
// time against a presented token.
func HashAuthToken(token []byte) string {
	sum := sha256.Sum256(token)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func VerifyAuthToken(token []byte, storedHashB64 string) bool {
	stored, err := base64.StdEncoding.DecodeString(storedHashB64)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(token)
	return subtle.ConstantTimeCompare(sum[:], stored) == 1
}
