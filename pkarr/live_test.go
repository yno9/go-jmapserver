package pkarr

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
)

// TestLiveDHT exercises the gateway against the REAL BitTorrent Mainline DHT:
// it publishes a signed record and reads it back. Gated on PKARR_LIVE=1 because
// it needs outbound UDP and can take tens of seconds to propagate. Run:
//
//	PKARR_LIVE=1 go test ./pkarr/ -run TestLiveDHT -v -timeout 180s
//
// It prints the z-base-32 key so you can independently confirm propagation via a
// public relay, e.g.:
//
//	curl -sv https://relay.pkarr.org/<printed-key> | xxd | head
func TestLiveDHT(t *testing.T) {
	if os.Getenv("PKARR_LIVE") != "1" {
		t.Skip("set PKARR_LIVE=1 to run the live mainline-DHT round-trip")
	}

	gw, err := NewGateway(nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	defer gw.Close()

	// Ephemeral identity + a signed payload, exactly as the biset client builds
	// it (sign the BEP44 canonical buffer over an empty salt).
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var pubkey [32]byte
	copy(pubkey[:], pub)

	raw := []byte("biset-live-dht-selftest-" + time.Now().Format(time.RFC3339))
	seq := time.Now().Unix()
	payload := signLive(priv, seq, raw)

	key := zbase32Encode(pub)
	t.Logf("publishing did:dht:%s (%d-byte packet, seq=%d)", key, len(raw), seq)
	t.Logf("verify externally: curl -s https://relay.pkarr.org/%s | xxd | head", key)

	putCtx, cancelPut := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPut()
	if err := gw.Put(putCtx, pubkey, payload); err != nil {
		t.Fatalf("Put to DHT failed: %v", err)
	}
	t.Log("Put OK — record announced to the DHT")

	// Propagation isn't instant; poll Get a few times.
	var got []byte
	for attempt := 1; attempt <= 6; attempt++ {
		getCtx, cancelGet := context.WithTimeout(context.Background(), 20*time.Second)
		got, err = gw.Get(getCtx, pubkey)
		cancelGet()
		if err == nil && got != nil {
			break
		}
		t.Logf("Get attempt %d: not yet (err=%v)", attempt, err)
		time.Sleep(5 * time.Second)
	}
	if got == nil {
		t.Fatal("Get returned no record after retries — DHT round-trip failed")
	}

	// Round-trip must return exactly what we published.
	sig, gotSeq, gotRaw, err := splitPayload(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotSeq != seq || string(gotRaw) != string(raw) {
		t.Fatalf("round-trip mismatch: seq=%d raw=%q", gotSeq, gotRaw)
	}
	if [64]byte(sig) != [64]byte(payload[:sigLen]) {
		t.Fatal("signature mismatch on round-trip")
	}
	t.Logf("Get OK — DHT round-trip verified (seq=%d, %d-byte packet)", gotSeq, len(gotRaw))
}

func signLive(priv ed25519.PrivateKey, seq int64, raw []byte) []byte {
	bv := bencode.MustMarshal(raw)
	buf := append([]byte(fmt.Sprintf("3:seqi%de1:v", seq)), bv...)
	sig := ed25519.Sign(priv, buf)
	payload := make([]byte, headerLen+len(raw))
	copy(payload[:sigLen], sig)
	binary.BigEndian.PutUint64(payload[sigLen:headerLen], uint64(seq))
	copy(payload[headerLen:], raw)
	return payload
}
