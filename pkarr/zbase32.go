// Package pkarr is a Pkarr-relay-format gateway: an HTTP bridge that lets
// browser clients (which cannot speak UDP/DHT) publish and resolve signed
// mutable records on the BitTorrent Mainline DHT. It is a shared capability of
// every relay binary (see biset DID.md), keeping the no-core design: the DHT
// holds ground truth, the gateway only relays and cannot forge (records are
// self-signed; a gateway can withhold or serve stale, never fabricate).
package pkarr

import "errors"

// z-base-32 (Zooko), the encoding did:dht/Pkarr use for the public-key path
// segment. Byte-identical to biset's src/did/zbase32.ts and the Rust
// base32::Alphabet::Z used by pkarr itself.
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

func zbase32Encode(b []byte) string {
	var out []byte
	bits, value := 0, 0
	for _, by := range b {
		value = (value << 8) | int(by)
		bits += 8
		for bits >= 5 {
			out = append(out, zAlphabet[(value>>(bits-5))&31])
			bits -= 5
		}
	}
	if bits > 0 {
		out = append(out, zAlphabet[(value<<(5-bits))&31])
	}
	return string(out)
}

// zbase32Decode decodes exactly n bytes (32 for an ed25519 public key),
// discarding the sub-byte padding bits the encoder appended.
func zbase32Decode(s string, n int) ([]byte, error) {
	out := make([]byte, 0, n)
	bits, value := 0, 0
	for i := 0; i < len(s); i++ {
		v := zInverse[s[i]]
		if v < 0 {
			return nil, errors.New("invalid z-base-32 character")
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
		return nil, errors.New("z-base-32 wrong decoded length")
	}
	return out, nil
}
