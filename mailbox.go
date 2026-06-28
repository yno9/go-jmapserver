package jmapserver

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
)

// HandleMailboxGet implements Mailbox/get: returns Mailboxes from the store.
func (s *Store) HandleMailboxGet(accountID jmap.ID, args json.RawMessage) (any, error) {
	mbs := s.Mailboxes()
	if mbs == nil {
		mbs = []mailbox.Mailbox{}
	}
	return map[string]any{
		"accountId": accountID,
		"state":     s.MailboxState(),
		"list":      mbs,
		"notFound":  []string{},
	}, nil
}

// HandleMailboxChanges implements Mailbox/changes with proper state tracking.
func (s *Store) HandleMailboxChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
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
	cur := s.mailboxState
	if since > cur {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}

	createdSet := map[jmap.ID]bool{}
	updatedSet := map[jmap.ID]bool{}
	destroyedSet := map[jmap.ID]bool{}
	for v := since + 1; v <= cur; v++ {
		rec, ok := s.mailboxChanges[v]
		if !ok {
			return nil, fmt.Errorf("cannotCalculateChanges")
		}
		for _, id := range rec.Created {
			createdSet[id] = true
			delete(destroyedSet, id)
		}
		for _, id := range rec.Updated {
			if !createdSet[id] {
				updatedSet[id] = true
			}
		}
		for _, id := range rec.Destroyed {
			destroyedSet[id] = true
			delete(createdSet, id)
			delete(updatedSet, id)
		}
	}

	return map[string]any{
		"accountId":      accountID,
		"oldState":       req.SinceState,
		"newState":       strconv.FormatInt(cur, 10),
		"hasMoreChanges": false,
		"created":        idsFromSet(createdSet),
		"updated":        idsFromSet(updatedSet),
		"destroyed":      idsFromSet(destroyedSet),
	}, nil
}

// HandleMailboxQuery implements Mailbox/query: returns filtered mailbox IDs.
func (s *Store) HandleMailboxQuery(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Filter *struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"filter"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	mbs := s.Mailboxes()
	var ids []jmap.ID
	for _, mb := range mbs {
		if req.Filter != nil {
			if req.Filter.Name != "" && !strings.Contains(strings.ToLower(mb.Name), strings.ToLower(req.Filter.Name)) {
				continue
			}
			if req.Filter.Role != "" && string(mb.Role) != req.Filter.Role {
				continue
			}
		}
		ids = append(ids, mb.ID)
	}
	if ids == nil {
		ids = []jmap.ID{}
	}
	return map[string]any{
		"accountId":           accountID,
		"queryState":          s.MailboxState(),
		"canCalculateChanges": true,
		"position":            0,
		"ids":                 ids,
		"total":               len(ids),
	}, nil
}

// HandleMailboxQueryChanges implements Mailbox/queryChanges with state tracking.
func (s *Store) HandleMailboxQueryChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		SinceQueryState string `json:"sinceQueryState"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	s.mu.RLock()
	defer s.mu.RUnlock()

	since, err := strconv.ParseInt(req.SinceQueryState, 10, 64)
	if err != nil || since < 0 {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}
	cur := s.mailboxState
	if since > cur {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}

	addedSet := map[jmap.ID]bool{}
	removedSet := map[jmap.ID]bool{}
	for v := since + 1; v <= cur; v++ {
		rec, ok := s.mailboxChanges[v]
		if !ok {
			return nil, fmt.Errorf("cannotCalculateChanges")
		}
		for _, id := range rec.Created {
			addedSet[id] = true
			delete(removedSet, id)
		}
		for _, id := range rec.Destroyed {
			removedSet[id] = true
			delete(addedSet, id)
		}
	}

	var added []map[string]any
	i := 0
	for id := range addedSet {
		added = append(added, map[string]any{"id": id, "index": i})
		i++
	}
	removed := idsFromSet(removedSet)
	if added == nil {
		added = []map[string]any{}
	}
	if removed == nil {
		removed = []jmap.ID{}
	}
	return map[string]any{
		"accountId":     accountID,
		"oldQueryState": req.SinceQueryState,
		"newQueryState": strconv.FormatInt(cur, 10),
		"removed":       removed,
		"added":         added,
	}, nil
}

// HandleMailboxSet implements Mailbox/set: create/update/destroy mailboxes.
func (s *Store) HandleMailboxSet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Create  map[jmap.ID]mailbox.Mailbox `json:"create"`
		Update  map[jmap.ID]json.RawMessage `json:"update"`
		Destroy []jmap.ID                   `json:"destroy"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	oldState := s.MailboxState()
	mbs := s.Mailboxes()
	mbByID := make(map[jmap.ID]*mailbox.Mailbox, len(mbs))
	for i := range mbs {
		mbByID[mbs[i].ID] = &mbs[i]
	}

	created := map[jmap.ID]any{}
	notCreated := map[jmap.ID]any{}
	updated := map[jmap.ID]any{}
	notUpdated := map[jmap.ID]any{}
	destroyed := []jmap.ID{}
	notDestroyed := map[jmap.ID]any{}

	var changeRec mailboxChangeRecord
	changed := false

	for key, mb := range req.Create {
		if mb.ID == "" {
			mb.ID = jmap.ID("mbx-" + string(key))
		}
		if s.onSetMailbox != nil {
			if err := s.onSetMailbox("create", mb.ID, &mb); err != nil {
				notCreated[key] = errObj("serverFail", err.Error())
				continue
			}
		}
		mbs = append(mbs, mb)
		mbByID[mb.ID] = &mb
		created[key] = map[string]any{"id": mb.ID}
		changeRec.Created = append(changeRec.Created, mb.ID)
		changed = true
	}

	for mbID, rawPatch := range req.Update {
		if _, ok := mbByID[mbID]; !ok {
			notUpdated[mbID] = errObj("notFound", "mailbox not found")
			continue
		}
		var patch map[string]any
		if err := json.Unmarshal(rawPatch, &patch); err != nil {
			notUpdated[mbID] = errObj("invalidProperties", err.Error())
			continue
		}
		mb := mbByID[mbID]
		if name, ok := patch["name"].(string); ok {
			mb.Name = name
		}
		updated[mbID] = map[string]any{}
		changeRec.Updated = append(changeRec.Updated, mbID)
		changed = true
	}

	destroySet := map[jmap.ID]bool{}
	for _, mbID := range req.Destroy {
		if _, ok := mbByID[mbID]; !ok {
			notDestroyed[mbID] = errObj("notFound", "mailbox not found")
			continue
		}
		if s.onSetMailbox != nil {
			if err := s.onSetMailbox("destroy", mbID, nil); err != nil {
				notDestroyed[mbID] = errObj("serverFail", err.Error())
				continue
			}
		}
		destroySet[mbID] = true
		destroyed = append(destroyed, mbID)
		changeRec.Destroyed = append(changeRec.Destroyed, mbID)
		changed = true
	}

	if changed {
		var next []mailbox.Mailbox
		for _, mb := range mbs {
			if !destroySet[mb.ID] {
				next = append(next, mb)
			}
		}
		s.PutMailboxes(next) //nolint:errcheck
		s.bumpMailboxState(changeRec)
	}

	return map[string]any{
		"accountId":    accountID,
		"oldState":     oldState,
		"newState":     s.MailboxState(),
		"created":      created,
		"updated":      updated,
		"destroyed":    destroyed,
		"notCreated":   notCreated,
		"notUpdated":   notUpdated,
		"notDestroyed": notDestroyed,
	}, nil
}
