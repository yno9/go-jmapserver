// Command pushtest sends one real Web Push notification to every subscription
// in a relay's push_subs.json, using the given VAPID keypair. Useful to check
// a deployment's VAPID config/connectivity in isolation from the rest of the
// relay (e.g. without needing a real message delivery to trigger Notify()).
//
// Usage: go run ./cmd/pushtest -data <dataDir> -pub <vapidPublicKey> -priv <vapidPrivateKey> -sub <you@example.com>
package main

import (
	"flag"
	"log"
	"time"

	jmapserver "github.com/yno9/go-jmapserver"
)

func main() {
	dataDir := flag.String("data", "", "relay data dir containing push_subs.json")
	pub := flag.String("pub", "", "VAPID public key")
	priv := flag.String("priv", "", "VAPID private key")
	sub := flag.String("sub", "", "VAPID subscriber contact — bare email (no mailto: prefix) or https: URL — required, some push services (Apple's) 403 without it")
	flag.Parse()
	if *dataDir == "" || *pub == "" || *priv == "" || *sub == "" {
		log.Fatal("usage: pushtest -data <dir> -pub <key> -priv <key> -sub <mailto:you@example.com>")
	}

	hub := jmapserver.NewHub()
	hub.SetVAPIDKeys(*pub, *priv, *sub)
	hub.SetPersistDir(*dataDir)
	n := hub.PushSubscriptionCount()
	log.Printf("loaded %d subscription(s) from %s — firing Notify()", n, *dataDir)
	hub.Notify()
	// Notify's push fan-out runs in a background goroutine, one HTTP round
	// trip at a time — Notify() itself doesn't block on it (by design: a
	// relay calling it from a request-handling path shouldn't stall on
	// network I/O to a third-party push service). Wait long enough for all
	// of them, not a fixed guess.
	time.Sleep(time.Duration(n+1) * 2 * time.Second)
	log.Println("done")
}
