package pkarr

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/anacrolix/dht/v2/bep44"
	"github.com/anacrolix/torrent/bencode"
)

// z-base-32 must match biset's client (src/did/zbase32.ts) and the Go PoC byte
// for byte, or DIDs computed on the two sides won't line up. This vector is the
// ed25519 public key derived from the fixed seed byte(i*7+3), whose DID the PoC
// printed as did:dht:qiqr3qjfp1uh5tfc9zdc95s4o1ebx3p39few7ge3dxm8hnapej5y.
func TestZbase32Vector(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i*7 + 3)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	const want = "qiqr3qjfp1uh5tfc9zdc95s4o1ebx3p39few7ge3dxm8hnapej5y"
	if got := zbase32Encode(pub); got != want {
		t.Fatalf("zbase32Encode = %q, want %q", got, want)
	}
	dec, err := zbase32Decode(want, 32)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(dec, pub) {
		t.Fatalf("decode round-trip mismatch")
	}
}

func TestZbase32RoundTrip(t *testing.T) {
	for n := 1; n <= 40; n++ {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*31 + n)
		}
		s := zbase32Encode(b)
		got, err := zbase32Decode(s, n)
		if err != nil {
			t.Fatalf("n=%d decode: %v", n, err)
		}
		if !bytes.Equal(got, b) {
			t.Fatalf("n=%d round-trip mismatch", n)
		}
	}
}

// A payload signed the way biset's client signs (ed25519 over the BEP44
// canonical buffer, empty salt) must pass the gateway's verification — the same
// bep44.Verify the Put path uses. This is the Go↔client agreement check, minus
// the live DHT.
func signClientPayload(priv ed25519.PrivateKey, seq int64, raw []byte) []byte {
	bv := bencode.MustMarshal(raw) // "<len>:<raw>"
	buf := append([]byte(fmt.Sprintf("3:seqi%de1:v", seq)), bv...)
	sig := ed25519.Sign(priv, buf)
	payload := make([]byte, headerLen+len(raw))
	copy(payload[:sigLen], sig)
	binary.BigEndian.PutUint64(payload[sigLen:headerLen], uint64(seq))
	copy(payload[headerLen:], raw)
	return payload
}

func TestPayloadVerifyAndSplit(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i*3 + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	var pubkey [32]byte
	copy(pubkey[:], priv.Public().(ed25519.PublicKey))

	raw := []byte("a-compressed-dns-packet-stand-in")
	var seq int64 = 1720000123
	payload := signClientPayload(priv, seq, raw)

	// splitPayload recovers the three fields.
	sig, gotSeq, gotRaw, err := splitPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotSeq != seq || !bytes.Equal(gotRaw, raw) {
		t.Fatalf("split mismatch: seq=%d raw=%q", gotSeq, gotRaw)
	}

	// The gateway's verification (identical call to Put's) must accept it.
	bv := bencode.MustMarshal(gotRaw)
	if !bep44.Verify(pubkey[:], nil, gotSeq, bv, sig[:]) {
		t.Fatal("gateway verification rejected a valid client payload")
	}

	// Tampering the packet must fail verification.
	bad := make([]byte, len(payload))
	copy(bad, payload)
	bad[headerLen] ^= 0x01
	s2, q2, r2, _ := splitPayload(bad)
	if bep44.Verify(pubkey[:], nil, q2, bencode.MustMarshal(r2), s2[:]) {
		t.Fatal("tampered payload passed verification")
	}
}

func TestSplitPayloadTooShort(t *testing.T) {
	if _, _, _, err := splitPayload(make([]byte, headerLen-1)); err == nil {
		t.Fatal("expected error for short payload")
	}
}
