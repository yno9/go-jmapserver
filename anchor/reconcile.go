package anchor

// Name is one address a relay hosts: the pair the anchor keys a claim by. The
// relay stores no DID for it — that lives on the anchor now — and Drain needs
// no DID anyway, since the anchor reads it off the claim it is about to release.
type Name struct {
	Localpart string `json:"localpart"`
	Domain    string `json:"domain"`
}

// DrainReport splits names by what an operator needs to know before turning a
// relay anchorless. Released names are confirmed clear at the anchor (whether or
// not they carried a claim — Release is idempotent). Failed names could not be
// reached or were refused, so their claims may still stand: a non-empty Failed
// means it is NOT yet safe to remove anchor_url.
type DrainReport struct {
	Released []Name `json:"released"`
	Failed   []Name `json:"failed"`
}

// Drain withdraws the identity claim for every name it is given — the bulk
// counterpart to Release, for the one reconciliation a relay can drive on its
// own: turning an anchored relay anchorless without stranding its names at the
// anchor. A claim left behind blocks a legitimately different relay from ever
// taking that name, and keeps the anchor announcing an identity nobody serves.
//
// This is the ONLY relay-driven reconciliation there is. The other direction,
// anchorless→anchored, cannot happen here: a claim needs a DID and a signature
// over it, and the relay holds neither — the key is the client's, and the
// envelope on disk is sealed to it. That direction is lazy and per-client: it
// happens when a client next logs in and calls PUT /account/did with a proof it
// computes locally, so an anchorless→anchored flip needs no server-side backfill
// at all, only for the relay to start accepting DIDs again.
//
// Drain must run while the anchor is still configured: releasing is an
// authenticated call TO the anchor, so it cannot happen after anchor_url is
// removed. Drive it, confirm Failed is empty, then go anchorless.
//
// Scope is exactly the names passed in — a relay's own hosted addresses. That
// boundary matters when relays share a domain (an operator's mail + ap under one
// @domain claim through one anchor): draining one of them releases claims the
// other still relies on. Anchored and anchorless must not be mixed across relays
// serving the same domain, the same invariant that makes an anchor per-operator.
func Drain(ref Ref, names []Name) DrainReport {
	rep := DrainReport{Released: []Name{}, Failed: []Name{}}
	for _, n := range names {
		if releaseOK(ref, n.Localpart, n.Domain) {
			rep.Released = append(rep.Released, n)
		} else {
			rep.Failed = append(rep.Failed, n)
		}
	}
	return rep
}
