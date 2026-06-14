package jmapserver

import (
	"encoding/json"
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
		"state":     "0",
		"list":      mbs,
		"notFound":  []string{},
	}, nil
}

// HandleMailboxChanges implements Mailbox/changes.
// Mailbox list is static (config-driven), so this always returns empty changes.
func (s *Store) HandleMailboxChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		SinceState string `json:"sinceState"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck
	return map[string]any{
		"accountId":      accountID,
		"oldState":       req.SinceState,
		"newState":       req.SinceState,
		"hasMoreChanges": false,
		"created":        []jmap.ID{},
		"updated":        []jmap.ID{},
		"destroyed":      []jmap.ID{},
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
		"queryState":          "0",
		"canCalculateChanges": false,
		"position":            0,
		"ids":                 ids,
		"total":               len(ids),
	}, nil
}

// HandleMailboxQueryChanges implements Mailbox/queryChanges.
// Mailboxes are static (config-driven), so always returns empty changes.
func (s *Store) HandleMailboxQueryChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		SinceQueryState string `json:"sinceQueryState"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck
	return map[string]any{
		"accountId":     accountID,
		"oldQueryState": req.SinceQueryState,
		"newQueryState": req.SinceQueryState,
		"removed":       []jmap.ID{},
		"added":         []map[string]any{},
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

	for key, mb := range req.Create {
		if mb.ID == "" {
			mb.ID = jmap.ID("mbx-" + string(key))
		}
		mbs = append(mbs, mb)
		mbByID[mb.ID] = &mb
		created[key] = map[string]any{"id": mb.ID}
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
	}

	destroySet := map[jmap.ID]bool{}
	for _, mbID := range req.Destroy {
		if _, ok := mbByID[mbID]; !ok {
			notDestroyed[mbID] = errObj("notFound", "mailbox not found")
			continue
		}
		destroySet[mbID] = true
		destroyed = append(destroyed, mbID)
	}

	if len(req.Create) > 0 || len(req.Update) > 0 || len(destroySet) > 0 {
		var next []mailbox.Mailbox
		for _, mb := range mbs {
			if !destroySet[mb.ID] {
				next = append(next, mb)
			}
		}
		s.PutMailboxes(next) //nolint:errcheck
	}

	return map[string]any{
		"accountId":    accountID,
		"oldState":     "0",
		"newState":     "0",
		"created":      created,
		"updated":      updated,
		"destroyed":    destroyed,
		"notCreated":   notCreated,
		"notUpdated":   notUpdated,
		"notDestroyed": notDestroyed,
	}, nil
}
