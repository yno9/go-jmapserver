package jmapserver

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Signature-based DID→relay binding verification (biset DID.md third-party
// portability). A client proves control of a DID by signing, with the DID's ROOT
// key, the host-bound statement:
//
//	bind:<did>:<username>@<relayHost>:<unixSeconds>
//
// The relay verifies the signature against the DID's own public key (the
// z-base-32 suffix of did:dht:<key>), so no secret is ever revealed. Host-binding
// + a freshness window stop a captured signature from being replayed elsewhere.

const didBindWindow = 300 * time.Second

// zBase32 alphabet (Zooko) — identical to pkarr / biset client / go-jmapserver/pkarr.
const zAlphabet = "ybndrfg8ejkmcpqxot1uwisza345h769"

var zInverse = func() [256]int8 {
	var m [256]int8
	for i := range m {
		m[i] = -1
	}
	for i := 0; i < len(zAlphabet); i++ {
		m[zAlphabet[i]] = int8(i)
	}
	return m
}()

func zbase32Decode(s string, n int) ([]byte, error) {
	out := make([]byte, 0, n)
	bits, value := 0, 0
	for i := 0; i < len(s); i++ {
		v := zInverse[s[i]]
		if v < 0 {
			return nil, errors.New("invalid z-base-32")
		}
		value = (value << 5) | int(v)
		bits += 5
		if bits >= 8 {
			bits -= 8
			if len(out) < n {
				out = append(out, byte((value>>bits)&0xff))
			}
		}
	}
	if len(out) != n {
		return nil, errors.New("z-base-32 wrong length")
	}
	return out, nil
}

// DIDPublicKey recovers the ed25519 public key a did:dht identifier names.
func DIDPublicKey(did string) (ed25519.PublicKey, error) {
	suffix := strings.TrimPrefix(did, "did:dht:")
	if suffix == did || suffix == "" {
		return nil, errors.New("not a did:dht identifier")
	}
	pk, err := zbase32Decode(suffix, ed25519.PublicKeySize)
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(pk), nil
}

// VerifyDIDBinding checks that sigB64 is a valid root-key signature over the
// binding statement for (did, username, relayHost, ts), and that ts is fresh.
func VerifyDIDBinding(did, username, relayHost string, ts int64, sigB64 string) error {
	if d := time.Since(time.Unix(ts, 0)); d > didBindWindow || d < -didBindWindow {
		return errors.New("binding timestamp out of window")
	}
	pk, err := DIDPublicKey(did)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("bad signature encoding: %w", err)
	}
	stmt := fmt.Sprintf("bind:%s:%s@%s:%d", did, username, relayHost, ts)
	if !ed25519.Verify(pk, []byte(stmt), sig) {
		return errors.New("binding signature invalid")
	}
	return nil
}

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
