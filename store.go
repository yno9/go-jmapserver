package jmapserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
)

// changeRecord records message IDs added, updated, or removed at a single state version.
type changeRecord struct {
	Added   []jmap.ID
	Updated []jmap.ID
	Removed []jmap.ID
}

type persistedState struct {
	State   int64                   `json:"state"`
	Changes map[string]changeRecord `json:"changes"`
}

// Store is a disk-backed, in-memory-cached JMAP mail object store.
//
// Disk layout:
//
//	<dir>/messages/<id>.json   — one file per Email object
//	<dir>/mailboxes.json       — Mailbox list
//	<dir>/delta.json           — queryState counter + change history
//
// Pending messages (Email/set drafts awaiting EmailSubmission/set) are
// held in memory only.
//
// queryState is a monotonic int64 counter persisted to delta.json.
// It survives restarts, enabling Email/queryChanges across relay restarts.
// If delta.json is absent or corrupted, state resets to 0 and clients
// receive cannotCalculateChanges on the next queryChanges call, causing
// them to fall back to a full Email/query fetch.
// CreateEmailFunc is called by HandleEmailSet for each Email/set create request.
// Return the created Email (with ID set) or an error.
type CreateEmailFunc func(raw json.RawMessage) (email.Email, error)

// SubmitEmailFunc is called by HandleEmailSubmissionSet for each create request.
// msg is the resolved Email; env is the submission envelope.
type SubmitEmailFunc func(msg email.Email, env emailsubmission.Envelope) error

// SetIdentityFunc is called after each Identity/set create, update, or destroy.
// op is "create", "update", or "destroy"; id is the identity ID; data is the identity object (nil on destroy).
// Return an error to reject the operation.
type SetIdentityFunc func(op string, id jmap.ID, data map[string]any) error

type Store struct {
	dir           string
	mu            sync.RWMutex
	msgs          map[jmap.ID]email.Email // persisted
	pending       map[jmap.ID]email.Email // in-memory only
	state         int64
	changes       map[int64]changeRecord
	stateFile     string
	identities    []map[string]any // persisted to identities.json
	identityState int64
	onCreate      CreateEmailFunc
	onSubmit      SubmitEmailFunc
	onSetIdentity SetIdentityFunc
	vacation      map[string]any // in-memory VacationResponse
}

// OnCreateEmail sets the hook called for Email/set create requests.
func (s *Store) OnCreateEmail(f CreateEmailFunc) { s.onCreate = f }

// OnSubmitEmail sets the hook called for EmailSubmission/set create requests.
func (s *Store) OnSubmitEmail(f SubmitEmailFunc) { s.onSubmit = f }

// OnSetIdentity sets the hook called after each Identity/set operation.
// Return an error to reject the operation.
func (s *Store) OnSetIdentity(f SetIdentityFunc) { s.onSetIdentity = f }

// NewStore opens (or creates) a store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "messages"), 0755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:       dir,
		msgs:      map[jmap.ID]email.Email{},
		pending:   map[jmap.ID]email.Email{},
		changes:   map[int64]changeRecord{},
		stateFile: filepath.Join(dir, "delta.json"),
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "messages"))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "messages", e.Name()))
		if err != nil {
			continue
		}
		var m email.Email
		if err := json.Unmarshal(b, &m); err == nil && m.ID != "" {
			s.msgs[m.ID] = m
		}
	}
	s.loadState()
	s.loadIdentities()
	return s, nil
}

// ── state persistence ─────────────────────────────────────────────────────────

func (s *Store) loadState() {
	b, err := os.ReadFile(s.stateFile)
	if err != nil {
		return
	}
	var ps persistedState
	if err := json.Unmarshal(b, &ps); err != nil {
		return
	}
	s.state = ps.State
	for k, v := range ps.Changes {
		n, err := strconv.ParseInt(k, 10, 64)
		if err == nil {
			s.changes[n] = v
		}
	}
}

func (s *Store) saveStateLocked() {
	ps := persistedState{
		State:   s.state,
		Changes: make(map[string]changeRecord, len(s.changes)),
	}
	for k, v := range s.changes {
		ps.Changes[strconv.FormatInt(k, 10)] = v
	}
	b, err := json.Marshal(ps)
	if err != nil {
		return
	}
	os.WriteFile(s.stateFile, b, 0644) //nolint:errcheck
}

// ── messages ──────────────────────────────────────────────────────────────────

// State returns the current queryState string.
func (s *Store) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strconv.FormatInt(s.state, 10)
}

// Put inserts or updates an Email on disk and in memory.
// Only new messages (not updates to existing ones) advance queryState.
// If the email has no ThreadID, one is resolved from In-Reply-To chains
// or generated fresh.
func (s *Store) Put(m email.Email) error {
	if m.ThreadID == "" {
		m.ThreadID = s.resolveThreadID(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.msgPath(m.ID), b, 0644); err != nil {
		return err
	}
	s.mu.Lock()
	_, exists := s.msgs[m.ID]
	s.msgs[m.ID] = m
	if !exists {
		s.state++
		s.changes[s.state] = changeRecord{Added: []jmap.ID{m.ID}}
		s.saveStateLocked()
	}
	s.mu.Unlock()
	return nil
}

// resolveThreadID finds an existing thread via In-Reply-To / References,
// or generates a new deterministic thread ID from the first Message-ID.
func (s *Store) resolveThreadID(m email.Email) jmap.ID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// build messageID → threadID index from stored messages
	byMsgID := make(map[string]jmap.ID, len(s.msgs))
	for _, stored := range s.msgs {
		for _, mid := range stored.MessageID {
			if mid != "" {
				byMsgID[mid] = stored.ThreadID
			}
		}
	}
	// walk In-Reply-To and References to find an existing thread
	refs := append(m.InReplyTo, m.References...)
	for _, ref := range refs {
		if tid, ok := byMsgID[ref]; ok && tid != "" {
			return tid
		}
	}
	// no parent found — start a new thread
	if len(m.MessageID) > 0 && m.MessageID[0] != "" {
		return jmap.ID("thr-" + m.MessageID[0])
	}
	return jmap.ID("thr-" + string(m.ID))
}

// Get returns an Email by ID, checking both persisted and pending stores.
func (s *Store) Get(id jmap.ID) (email.Email, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.msgs[id]; ok {
		return m, true
	}
	m, ok := s.pending[id]
	return m, ok
}

// Delete removes a persisted Email by ID.
func (s *Store) Delete(id jmap.ID) {
	s.mu.Lock()
	if _, exists := s.msgs[id]; exists {
		delete(s.msgs, id)
		s.state++
		s.changes[s.state] = changeRecord{Removed: []jmap.ID{id}}
		s.saveStateLocked()
	}
	s.mu.Unlock()
	os.Remove(s.msgPath(id)) //nolint:errcheck
}

// All returns all persisted Emails sorted newest-first by ReceivedAt.
func (s *Store) All() []email.Email {
	s.mu.RLock()
	all := make([]email.Email, 0, len(s.msgs))
	for _, m := range s.msgs {
		all = append(all, m)
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool {
		ti := timeVal(all[i].ReceivedAt)
		tj := timeVal(all[j].ReceivedAt)
		return ti.After(tj)
	})
	return all
}

// PatchKeywords applies a JMAP keyword patch (e.g. {"keywords/$seen": true})
// to a stored Email, persists the change, and records an Updated entry.
func (s *Store) PatchKeywords(id jmap.ID, patch map[string]any) error {
	s.mu.Lock()
	m, ok := s.msgs[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	cp := m
	if cp.Keywords == nil {
		cp.Keywords = map[string]bool{}
	}
	for k, v := range patch {
		if kw := strings.TrimPrefix(k, "keywords/"); kw != k {
			if b, isBool := v.(bool); isBool {
				cp.Keywords[kw] = b
			}
		}
	}
	s.msgs[id] = cp
	s.state++
	s.changes[s.state] = changeRecord{Updated: []jmap.ID{id}}
	s.saveStateLocked()
	s.mu.Unlock()

	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	return os.WriteFile(s.msgPath(id), b, 0644)
}

// ── pending ───────────────────────────────────────────────────────────────────

// PutPending stores a draft Email in memory only (not persisted to disk).
func (s *Store) PutPending(m email.Email) {
	s.mu.Lock()
	s.pending[m.ID] = m
	s.mu.Unlock()
}

// TakePending removes and returns a pending Email (called when submitting).
func (s *Store) TakePending(id jmap.ID) (email.Email, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	return m, ok
}

// ── mailboxes ─────────────────────────────────────────────────────────────────

// PutMailboxes overwrites the persisted Mailbox list.
func (s *Store) PutMailboxes(mbs []mailbox.Mailbox) error {
	b, err := json.Marshal(mbs)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "mailboxes.json"), b, 0644)
}

// Mailboxes returns the persisted Mailbox list.
func (s *Store) Mailboxes() []mailbox.Mailbox {
	b, err := os.ReadFile(filepath.Join(s.dir, "mailboxes.json"))
	if err != nil {
		return nil
	}
	var mbs []mailbox.Mailbox
	json.Unmarshal(b, &mbs) //nolint:errcheck
	return mbs
}

// ── identity persistence ──────────────────────────────────────────────────────

func (s *Store) identitiesPath() string {
	return filepath.Join(s.dir, "identities.json")
}

func (s *Store) loadIdentities() {
	b, err := os.ReadFile(s.identitiesPath())
	if err != nil {
		return
	}
	var ids []map[string]any
	if json.Unmarshal(b, &ids) == nil {
		s.identities = ids
	}
}

func (s *Store) saveIdentitiesLocked() {
	b, err := json.Marshal(s.identities)
	if err != nil {
		return
	}
	os.WriteFile(s.identitiesPath(), b, 0644) //nolint:errcheck
}

func (s *Store) defaultIdentity(accountID jmap.ID) map[string]any {
	addr := string(accountID)
	name := addr
	if at := strings.Index(addr, "@"); at > 0 {
		name = addr[:at]
	}
	return map[string]any{
		"id":            "identity-" + addr,
		"name":          name,
		"email":         addr,
		"replyTo":       nil,
		"bcc":           nil,
		"textSignature": "",
		"htmlSignature": "",
		"mayDelete":     false,
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func errObj(typ, desc string) map[string]string {
	return map[string]string{"type": typ, "description": desc}
}

func timeNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ── internal ──────────────────────────────────────────────────────────────────

func (s *Store) msgPath(id jmap.ID) string {
	return filepath.Join(s.dir, "messages", safeFilename(string(id))+".json")
}

func safeFilename(s string) string {
	rep := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "-", `"`, "-", "<", "-", ">", "-", "|", "-",
	)
	s = rep.Replace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func idsFromSet(set map[jmap.ID]bool) []jmap.ID {
	ids := make([]jmap.ID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

func timeVal(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
