// Command vapidgen prints a fresh VAPID keypair for Web Push, in the form
// expected by jmapserver.Config's vapid_public_key / vapid_private_key.
//
// Run once per relay deployment and paste the output into config.json — keep
// it stable afterward, since rotating it invalidates every client's existing
// subscription.
package main

import (
	"fmt"
	"log"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func main() {
	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		log.Fatalf("generate: %v", err)
	}
	fmt.Printf("\"vapid_public_key\": %q,\n\"vapid_private_key\": %q\n", public, private)
}
