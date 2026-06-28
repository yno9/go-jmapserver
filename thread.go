package jmapserver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
)

// Thread is a lightweight summary of a thread: its ID and the message IDs it
// contains in ReceivedAt order. Returned by AllThreads for callers that need
// to enumerate threads without parsing JMAP method responses.
type Thread struct {
	ID       jmap.ID   `json:"id"`
	EmailIDs []jmap.ID `json:"emailIds"`
}

// AllThreads returns every distinct ThreadID in the store with its member
// EmailIDs (sorted by ReceivedAt). Threads themselves are not persisted —
// this derives them from the message store on demand.
func (s *Store) AllThreads() []Thread {
	s.mu.RLock()
	byThread := map[jmap.ID][]email.Email{}
	for _, m := range s.msgs {
		if m.ThreadID == "" {
			continue
		}
		byThread[m.ThreadID] = append(byThread[m.ThreadID], m)
	}
	s.mu.RUnlock()
	out := make([]Thread, 0, len(byThread))
	for tid, msgs := range byThread {
		sort.Slice(msgs, func(i, j int) bool {
			return timeVal(msgs[i].ReceivedAt).Before(timeVal(msgs[j].ReceivedAt))
		})
		ids := make([]jmap.ID, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		out = append(out, Thread{ID: tid, EmailIDs: ids})
	}
	return out
}

// AllForThread returns all persisted Emails with the given ThreadID, sorted by ReceivedAt.
func (s *Store) AllForThread(threadID jmap.ID) []email.Email {
	s.mu.RLock()
	var out []email.Email
	for _, m := range s.msgs {
		if m.ThreadID == threadID {
			out = append(out, m)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return timeVal(out[i].ReceivedAt).Before(timeVal(out[j].ReceivedAt))
	})
	return out
}

// HandleThreadGet implements Thread/get: groups Emails by ThreadID and returns
// {id, emailIds} entries for each requested thread ID, sorted by ReceivedAt.
func (s *Store) HandleThreadGet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		IDs []jmap.ID `json:"ids"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	s.mu.RLock()
	type entry struct {
		id jmap.ID
		at time.Time
	}
	byThread := map[jmap.ID][]entry{}
	for _, m := range s.msgs {
		if m.ThreadID != "" {
			byThread[m.ThreadID] = append(byThread[m.ThreadID], entry{m.ID, timeVal(m.ReceivedAt)})
		}
	}
	s.mu.RUnlock()

	var list []map[string]any
	var notFound []jmap.ID
	for _, tid := range req.IDs {
		entries, ok := byThread[tid]
		if !ok {
			notFound = append(notFound, tid)
			continue
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].at.Before(entries[j].at)
		})
		emailIDs := make([]jmap.ID, len(entries))
		for i, e := range entries {
			emailIDs[i] = e.id
		}
		list = append(list, map[string]any{
			"id":       tid,
			"emailIds": emailIDs,
		})
	}
	if list == nil {
		list = []map[string]any{}
	}
	if notFound == nil {
		notFound = []jmap.ID{}
	}
	return map[string]any{
		"accountId": accountID,
		"state":     s.State(),
		"list":      list,
		"notFound":  notFound,
	}, nil
}

// HandleThreadChanges implements Thread/changes.
// Thread state mirrors email state; threads are considered changed when any of their emails changed.
func (s *Store) HandleThreadChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		SinceState string `json:"sinceState"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	s.mu.RLock()
	defer s.mu.RUnlock()

	since, err := strconv.ParseInt(req.SinceState, 10, 64)
	if err != nil || since < 0 {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}
	cur := s.state
	if since > cur {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}

	// collect all email IDs that changed
	changedEmails := map[jmap.ID]bool{}
	for v := since + 1; v <= cur; v++ {
		rec, ok := s.changes[v]
		if !ok {
			return nil, fmt.Errorf("cannotCalculateChanges")
		}
		for _, id := range rec.Added {
			changedEmails[id] = true
		}
		for _, id := range rec.Updated {
			changedEmails[id] = true
		}
		for _, id := range rec.Removed {
			changedEmails[id] = true
		}
	}

	// map email IDs → thread IDs
	changedThreads := map[jmap.ID]bool{}
	for emailID := range changedEmails {
		if m, ok := s.msgs[emailID]; ok && m.ThreadID != "" {
			changedThreads[m.ThreadID] = true
		}
	}

	updated := make([]jmap.ID, 0, len(changedThreads))
	for tid := range changedThreads {
		updated = append(updated, tid)
	}

	return map[string]any{
		"accountId":      accountID,
		"oldState":       req.SinceState,
		"newState":       strconv.FormatInt(cur, 10),
		"hasMoreChanges": false,
		"created":        []jmap.ID{},
		"updated":        updated,
		"destroyed":      []jmap.ID{},
	}, nil
}
