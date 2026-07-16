package pkarr

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/bep44"
	"github.com/anacrolix/dht/v2/exts/getput"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/torrent/bencode"
)

// Wire payload (Pkarr relay spec design/relays.md), also what biset's
// src/did/packet.ts produces/consumes:
//
//	signature(64) ‖ seq(8, big-endian) ‖ v (raw compressed DNS packet, <1000B)
//
// The signature is ed25519 over the BEP44 canonical buffer "3:seqi<seq>e1:v" ‖
// bencode(v), empty salt. In the DHT the value is stored bencoded (<len>:<v>);
// this gateway strips/adds that bencode wrapper so the HTTP body is the raw v.
const (
	sigLen        = 64
	seqLen        = 8
	headerLen     = sigLen + seqLen
	maxPacketLen  = 1000
	republishFreq = 30 * time.Minute
)

// Gateway wraps a Mainline DHT node and republishes the records of identities
// this relay serves, so a DID stays resolvable as long as one of its relays is
// up (see DID.md republish rules).
type Gateway struct {
	s *dht.Server

	mu      sync.Mutex
	cache   map[[32]byte][]byte // pubkey → last-seen full payload, for republishing
	closing chan struct{}
}

// NewGateway starts a DHT node. If conn is nil a UDP socket on an ephemeral port
// is opened. The caller owns shutdown via Close.
func NewGateway(conn net.PacketConn) (*Gateway, error) {
	if conn == nil {
		c, err := net.ListenPacket("udp", ":0")
		if err != nil {
			return nil, fmt.Errorf("pkarr: udp listen: %w", err)
		}
		conn = c
	}
	cfg := dht.NewDefaultServerConfig()
	cfg.Conn = conn
	s, err := dht.NewServer(cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("pkarr: dht server: %w", err)
	}
	g := &Gateway{s: s, cache: map[[32]byte][]byte{}, closing: make(chan struct{})}
	go g.republishLoop()
	return g, nil
}

func (g *Gateway) Close() {
	close(g.closing)
	g.s.Close()
}

// Get resolves a mutable record for pubkey and returns the reassembled wire
// payload (sig ‖ seq ‖ v). Returns (nil, nil) when the DHT has no such record.
func (g *Gateway) Get(ctx context.Context, pubkey [32]byte) ([]byte, error) {
	target := bep44.MakeMutableTarget(pubkey, nil)
	res, _, err := getput.Get(ctx, target, g.s, nil, nil)
	if err != nil {
		return nil, nil // not found / traversal stalled — treat as absent
	}
	// res.V is the bencoded value (<len>:<raw>); unwrap to the raw DNS packet.
	var raw []byte
	if err := bencode.Unmarshal(res.V, &raw); err != nil {
		return nil, fmt.Errorf("pkarr: value not a bencoded string: %w", err)
	}
	payload := make([]byte, headerLen+len(raw))
	copy(payload[:sigLen], res.Sig[:])
	binary.BigEndian.PutUint64(payload[sigLen:headerLen], uint64(res.Seq))
	copy(payload[headerLen:], raw)
	g.remember(pubkey, payload)
	return payload, nil
}

// Put validates a wire payload against pubkey and publishes it to the DHT. The
// gateway never signs — it forwards the client's signature, so it cannot forge.
func (g *Gateway) Put(ctx context.Context, pubkey [32]byte, payload []byte) error {
	sig, seq, raw, err := splitPayload(payload)
	if err != nil {
		return err
	}
	if len(raw) > maxPacketLen {
		return errors.New("pkarr: DNS packet exceeds 1000 bytes")
	}
	// bencode(raw) is exactly what was signed after the "…1:v" prefix.
	bv := bencode.MustMarshal(raw)
	if !bep44.Verify(pubkey[:], nil, seq, bv, sig[:]) {
		return errors.New("pkarr: invalid signature")
	}
	if err := g.put(ctx, pubkey, seq, sig, raw); err != nil {
		return err
	}
	g.remember(pubkey, payload)
	return nil
}

func (g *Gateway) put(ctx context.Context, pubkey [32]byte, seq int64, sig [64]byte, raw []byte) error {
	target := bep44.MakeMutableTarget(pubkey, nil)
	k := pubkey
	_, err := getput.Put(ctx, krpc.ID(target), g.s, nil, func(_ int64) bep44.Put {
		// Use the client's seq and signature verbatim (ignore the auto-seq the
		// traversal discovered — the record is signed for exactly this seq).
		return bep44.Put{V: raw, K: &k, Sig: sig, Seq: seq}
	})
	if err != nil {
		return fmt.Errorf("pkarr: dht put: %w", err)
	}
	return nil
}

func (g *Gateway) remember(pubkey [32]byte, payload []byte) {
	cp := make([]byte, len(payload))
	copy(cp, payload)
	g.mu.Lock()
	g.cache[pubkey] = cp
	g.mu.Unlock()
}

// Forget drops pubkey from the republish cache — call when the identity it
// belongs to is permanently deleted. Without this, republishLoop would keep
// re-announcing an orphaned record forever (nothing here ever expired an
// entry on its own): BEP44's "expiration" section only lets an unattended
// record fade in ~2 hours once nothing is left re-announcing it, and this
// gateway itself was that unattended re-announcer.
func (g *Gateway) Forget(pubkey [32]byte) {
	g.mu.Lock()
	delete(g.cache, pubkey)
	g.mu.Unlock()
}

// republishLoop re-puts every remembered record periodically; DHT records
// expire in hours, so a served identity must be refreshed to stay resolvable.
func (g *Gateway) republishLoop() {
	t := time.NewTicker(republishFreq)
	defer t.Stop()
	for {
		select {
		case <-g.closing:
			return
		case <-t.C:
			g.mu.Lock()
			snapshot := make(map[[32]byte][]byte, len(g.cache))
			for k, v := range g.cache {
				snapshot[k] = v
			}
			g.mu.Unlock()
			for pubkey, payload := range snapshot {
				sig, seq, raw, err := splitPayload(payload)
				if err != nil {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				g.put(ctx, pubkey, seq, sig, raw) //nolint:errcheck
				cancel()
			}
		}
	}
}

func splitPayload(payload []byte) (sig [64]byte, seq int64, raw []byte, err error) {
	if len(payload) < headerLen {
		err = errors.New("pkarr: payload too short")
		return
	}
	copy(sig[:], payload[:sigLen])
	seq = int64(binary.BigEndian.Uint64(payload[sigLen:headerLen]))
	raw = payload[headerLen:]
	return
}
