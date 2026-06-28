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

`hub` may be nil; if non-nil, `/jmap/eventsource/` SSE endpoint is added.

### Hub (`server.go`)

```go
hub := jmapserver.NewHub()
hub.Notify()   // broadcast state-change to all SSE subscribers
```

Call `hub.Notify()` whenever new data arrives (IMAP IDLE, HTTP POST, RSS poll, …). Connected clients receive a `state` SSE event and re-fetch.

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
1. Walk `InReplyTo` + `References` against stored messages' `MessageID`
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
