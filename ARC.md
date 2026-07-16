---
description: Architecture document for go-jmapserver. Read before modifying.
---

# go-jmapserver

A general-purpose JMAP server framework (RFC 8620/8621) used by biset and all its relays.

## Concept

go-jmapserver provides two independent building blocks:

- **Handler + Serve/NewMux** — HTTP wire protocol (session, API, SSE)
- **Store** — disk-backed JMAP object storage with method handlers

A relay or server implements `Handler`, creates a `Store`, and delegates JMAP method calls to `Store.Dispatch` (or individual `Handle*` methods). Protocol-specific logic runs outside the store, via hooks.

```
JMAP client (HTTP)
    ↓
Serve / NewMux          — /.well-known/jmap, /jmap/api/, /jmap/eventsource/
    ↓
Handler.Handle(method, args)
    ↓
Store.Dispatch(accountID, method, args)   — or individual Handle* calls
    ↓
Store (disk + memory)   — messages/, mailboxes.json, delta.json
```

---

## Handler interface (`server.go`)

```go
type Handler interface {
    Capabilities() []jmap.URI   // e.g. "urn:ietf:params:jmap:mail"
    Accounts() []Account        // one entry per JMAP account
    Handle(method string, args json.RawMessage) (any, error)
}
```

The implementor calls `Store.Dispatch` (or specific `Handle*` methods) from `Handle`. Methods that require protocol-specific side effects (e.g. IMAP fetch before `Email/query`) are intercepted before delegation; everything else passes through to `Dispatch`.

### Serving

```go
// Simple: blocks until error
jmapserver.Serve(cfg, handler, hub)

// Custom mux: add extra routes (WebDAV, ActivityPub, WKD, …)
mux := jmapserver.NewMux(cfg, handler, hub)
mux.HandleFunc("/my-route", myHandler)
http.ListenAndServe(addr, mux)
```

`hub` may be nil; if non-nil, `/jmap/eventsource/` SSE endpoint is added, along with the Web Push endpoints below.

### Hub (`server.go`)

```go
hub := jmapserver.NewHub()
hub.Notify()   // broadcast state-change to all SSE subscribers + registered Web Push subscriptions
```

Call `hub.Notify()` whenever new data arrives (IMAP IDLE, HTTP POST, RSS poll, …). Connected clients receive a `state` SSE event and re-fetch. `Notify()` also fans out to any Web Push subscriptions registered on the hub (see below) — this is the *only* thing a relay needs to call; there is no separate push-specific notify path.

### Web Push (`push.go`)

Piggybacks on `Notify()` so relays get background wake-ups for free — no relay-specific code needed beyond configuring VAPID keys and (optionally) persistence.

```go
hub.SetVAPIDKeys(cfg.VapidPublicKey, cfg.VapidPrivateKey)  // done automatically by NewMux/Serve if cfg carries them
hub.SetPersistDir(dataDir)                                 // optional: survive restarts (push_subs.json)
```

`Config.VapidPublicKey` / `VapidPrivateKey` (both `json:"vapid_public_key"` / `"vapid_private_key"`) are read by `NewMux`/`Serve`: if `VapidPublicKey` is non-empty, `hub.SetVAPIDKeys` is called automatically. Generate a pair with `go run ./cmd/vapidgen` — keep it stable (rotating invalidates every client subscription).

**Important**: an identity's push subscription lives at the *client's own origin*, not the relay's — one browser subscription, tied to one VAPID keypair. If an identity spans multiple relays (e.g. mail + ActivityPub on the same home domain), every relay it touches must be configured with the **same** VAPID keypair, or that relay's push sends will fail to authenticate against the subscription. VAPID keys are therefore scoped per client-app deployment (home domain), copied identically into each relay's config — not generated independently per relay.

Endpoints (added to `NewMux` when `hub != nil`):

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /jmap/push/vapid-public-key` | none | Serves the configured public key so the client can call `PushManager.subscribe({applicationServerKey})` |
| `POST /jmap/push/subscribe` | yes | Body `{endpoint, keys:{p256dh,auth}}` — registers a subscription against the authenticated accountID |
| `POST /jmap/push/unsubscribe` | yes | Body `{endpoint}` — removes it |

The push payload sent on `Notify()` is always empty. The relay only knows its own store's unread count, not an identity's total across every relay it has an account on — so the payload can't carry a badge number without risking one relay's push clobbering another's. Instead it's purely a wake-up signal: the client's Service Worker, which knows about every relay the identity uses, re-fetches and computes the true total itself before setting the badge. A subscription that 404s/410s on send is treated as gone and pruned automatically.

---

## Store (`store.go`, `email.go`, `mailbox.go`, `thread.go`, `identity.go`, `submission.go`)

Disk-backed, in-memory-cached JMAP mail object store.

### Disk layout

```
<dir>/
├── messages/<id>.json   — one file per Email object
├── mailboxes.json        — Mailbox list
├── delta.json            — monotonic state counter + change history
└── identities.json       — Identity list
```

`delta.json` survives restarts; enables `Email/queryChanges` across restarts. If absent or corrupted, state resets to 0 and clients receive `cannotCalculateChanges`, falling back to full `Email/query`.

### Core methods

```go
store, _ := jmapserver.NewStore(dir)

// Email
store.Put(m)                     // insert/update; resolves ThreadID from InReplyTo chain
store.Get(id)                    // lookup by ID (persisted + pending)
store.Delete(id)                 // remove from disk + memory
store.All()                      // all emails, newest-first
store.AllForThread(threadID)     // emails in thread, oldest-first
store.PatchKeywords(id, patch)   // apply keywords/* patch
store.PatchEmail(id, patch)      // apply keywords/* and mailboxIds/* patches

// Mailboxes
store.PutMailboxes(mbs)          // overwrite list
store.Mailboxes()                // read list

// Pending (in-memory drafts, not persisted)
store.PutPending(m)
store.TakePending(id)

// State
store.State()                    // current queryState string
```

### Thread ID resolution (`Put`)

`Put` resolves `ThreadID` automatically if not set:
0. If a `Chat-Group-Id` header is present (DeltaChat group message — preserved on `Email.Headers` like any other non-standard header, see email.go): `"thr-group-" + that value`, full stop, skipping the walk below entirely. A group is a flat chat with no threading concept — there's no "reply to a specific message" UI, just one continuous stream — so thread id should just BE the group id. Doing this via reply-chain-walking instead is actively wrong, not just redundant: DeltaChat splits one logical text+image message into two separate MIME messages that both reference a common parent, and if that parent was never delivered to (or already pruned from) this relay, each half independently falls through to step 3 below and mints its own thread id — one message visibly split into two threads client-side.
1. Otherwise, walk `InReplyTo` + `References` against stored messages' `MessageID`
2. If match found: inherit that thread's ID
3. Otherwise: `"thr-" + MessageID[0]` (or `"thr-" + ID` as fallback)

### JMAP method handlers

All RFC 8621 methods are implemented as `Handle*` methods on Store.

| Method | Notes |
|---|---|
| `Email/get` | lookup by IDs |
| `Email/query` | filter by `inMailbox`, `position`, `limit` |
| `Email/changes` | delta from `sinceState` |
| `Email/queryChanges` | delta from `sinceQueryState` |
| `Email/set` | create → `OnCreateEmail` hook; update → `PatchEmail`; destroy → `Delete` |
| `Email/copy` | serverFail (not supported) |
| `Email/import` | serverFail (not supported) |
| `Email/parse` | serverFail (not supported) |
| `SearchSnippet/get` | plain-text body search |
| `Thread/get` | emailIds sorted by ReceivedAt asc |
| `Thread/changes` | mirrors email state changes |
| `Mailbox/get` | from `mailboxes.json` |
| `Mailbox/changes` | always empty (mailboxes are config-driven; dynamic changes via `Mailbox/set`) |
| `Mailbox/query` | filter by name, role |
| `Mailbox/queryChanges` | always empty |
| `Mailbox/set` | create/update/destroy; calls `OnSetMailbox` hook |
| `Identity/get` | from `identities.json`; fallback default from accountID |
| `Identity/changes` | delta tracking |
| `Identity/set` | create/update/destroy; calls `OnSetIdentity` hook |
| `EmailSubmission/get` | always empty |
| `EmailSubmission/changes` | always empty |
| `EmailSubmission/query` | always empty |
| `EmailSubmission/queryChanges` | always empty |
| `EmailSubmission/set` | resolve email from store/pending; calls `OnSubmitEmail` hook |
| `VacationResponse/get` | in-memory, default disabled |
| `VacationResponse/set` | in-memory update |

`Dispatch` routes all of the above automatically.

### Hooks

Hooks inject protocol-specific behavior into standard JMAP operations.

```go
// Email/set create — return created Email or error
store.OnCreateEmail(func(raw json.RawMessage) (email.Email, error) { ... })

// EmailSubmission/set create — called with resolved Email and Envelope
store.OnSubmitEmail(func(msg email.Email, env emailsubmission.Envelope) error { ... })

// Mailbox/set create or destroy — op is "create" or "destroy"; mb is nil on destroy
store.OnSetMailbox(func(op string, id jmap.ID, mb *mailbox.Mailbox) error { ... })

// Identity/set create, update, or destroy
store.OnSetIdentity(func(op string, id jmap.ID, data map[string]any) error { ... })

// Email/set destroy — called before Delete; return error to reject
store.OnDestroyEmail(func(id jmap.ID) error { ... })

// Email/set update — called before PatchEmail; return error to reject
store.OnUpdateEmail(func(id jmap.ID, patch map[string]any) error { ... })
```

Return an error from any hook to reject the operation (response: `serverFail`).

---

## Usage pattern

Every relay and biset server follows the same pattern:

```go
store, _ := jmapserver.NewStore(dataDir)
hub := jmapserver.NewHub()

// wire protocol-specific hooks
store.OnSubmitEmail(func(msg email.Email, env emailsubmission.Envelope) error {
    return smtpSend(msg, env)  // relay-specific
})
store.OnCreateEmail(func(raw json.RawMessage) (email.Email, error) {
    // parse, store pending, return draft email
})

// implement Handler
type myHandler struct{ store *jmapserver.Store }
func (h *myHandler) Capabilities() []jmap.URI { ... }
func (h *myHandler) Accounts() []jmapserver.Account { ... }
func (h *myHandler) Handle(method string, args json.RawMessage) (any, error) {
    switch method {
    case "Email/query":
        fetchNewFromProtocol()     // protocol-specific side effect
        return h.store.HandleEmailQuery(accountID, args)
    default:
        return h.store.Dispatch(accountID, method, args)
    }
}

jmapserver.Serve(cfg, &myHandler{store}, hub)
```
