package jmapserver

import (
	"encoding/json"
	"fmt"
	"strconv"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

// HandleEmailGet implements Email/get: returns Emails by ID.
func (s *Store) HandleEmailGet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		IDs []jmap.ID `json:"ids"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	var list []any
	var notFound []jmap.ID
	for _, id := range req.IDs {
		if m, ok := s.Get(id); ok {
			list = append(list, m)
		} else {
			notFound = append(notFound, id)
		}
	}
	if list == nil {
		list = []any{}
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

// HandleEmailQuery implements Email/query with optional filter (inMailbox), position, and limit.
func (s *Store) HandleEmailQuery(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Filter   *struct{ InMailbox string `json:"inMailbox"` } `json:"filter"`
		Position int                                            `json:"position"`
		Limit    uint64                                         `json:"limit"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	all := s.All() // sorted newest-first
	var filtered []jmap.ID
	for _, m := range all {
		if req.Filter != nil && req.Filter.InMailbox != "" {
			if !m.MailboxIDs[jmap.ID(req.Filter.InMailbox)] {
				continue
			}
		}
		filtered = append(filtered, m.ID)
	}

	total := len(filtered)
	start := req.Position
	if start < 0 {
		start = 0
	}
	if start >= total {
		return map[string]any{
			"accountId": accountID, "queryState": s.State(),
			"canCalculateChanges": true, "position": start,
			"ids": []jmap.ID{}, "total": total,
		}, nil
	}
	end := total
	if req.Limit > 0 && int(req.Limit) < end-start {
		end = start + int(req.Limit)
	}
	return map[string]any{
		"accountId":           accountID,
		"queryState":          s.State(),
		"canCalculateChanges": true,
		"position":            start,
		"ids":                 filtered[start:end],
		"total":               total,
	}, nil
}

// HandleEmailChanges implements Email/changes: returns object-level changes
// (created, updated, destroyed) since sinceState.
func (s *Store) HandleEmailChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
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

	createdSet := map[jmap.ID]bool{}
	updatedSet := map[jmap.ID]bool{}
	destroyedSet := map[jmap.ID]bool{}
	for v := since + 1; v <= cur; v++ {
		rec, ok := s.changes[v]
		if !ok {
			return nil, fmt.Errorf("cannotCalculateChanges")
		}
		for _, id := range rec.Added {
			createdSet[id] = true
			delete(destroyedSet, id)
		}
		for _, id := range rec.Updated {
			if !createdSet[id] {
				updatedSet[id] = true
			}
		}
		for _, id := range rec.Removed {
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

// HandleQueryChanges implements Email/queryChanges: returns query-level changes
// (added to / removed from the result set) since sinceQueryState.
func (s *Store) HandleQueryChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
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
	cur := s.state
	if since > cur {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}

	addedSet := map[jmap.ID]bool{}
	removedSet := map[jmap.ID]bool{}
	for v := since + 1; v <= cur; v++ {
		rec, ok := s.changes[v]
		if !ok {
			return nil, fmt.Errorf("cannotCalculateChanges")
		}
		for _, id := range rec.Added {
			addedSet[id] = true
			delete(removedSet, id)
		}
		for _, id := range rec.Removed {
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
	var removed []jmap.ID
	for id := range removedSet {
		removed = append(removed, id)
	}
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

// HandleEmailSet implements Email/set.
// create: calls OnCreateEmail hook; if not set, returns serverFail.
// update: applies keyword patches via PatchKeywords.
// destroy: calls Delete.
func (s *Store) HandleEmailSet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Create  map[jmap.ID]json.RawMessage `json:"create"`
		Update  map[jmap.ID]json.RawMessage `json:"update"`
		Destroy []jmap.ID                   `json:"destroy"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	oldState := s.State()
	created := map[jmap.ID]any{}
	notCreated := map[jmap.ID]any{}
	updated := map[jmap.ID]any{}
	notUpdated := map[jmap.ID]any{}
	destroyed := []jmap.ID{}
	notDestroyed := map[jmap.ID]any{}

	for key, raw := range req.Create {
		if s.onCreate == nil {
			notCreated[key] = errObj("serverFail", "Email/set create not configured")
			continue
		}
		m, err := s.onCreate(raw)
		if err != nil {
			notCreated[key] = errObj("serverFail", err.Error())
			continue
		}
		created[key] = map[string]any{"id": m.ID}
	}

	for msgID, rawPatch := range req.Update {
		var patch map[string]any
		if err := json.Unmarshal(rawPatch, &patch); err != nil {
			notUpdated[msgID] = errObj("invalidProperties", err.Error())
			continue
		}
		if s.onUpdateEmail != nil {
			if err := s.onUpdateEmail(msgID, patch); err != nil {
				notUpdated[msgID] = errObj("serverFail", err.Error())
				continue
			}
		}
		if err := s.PatchEmail(msgID, patch); err != nil {
			notUpdated[msgID] = errObj("serverFail", err.Error())
			continue
		}
		updated[msgID] = map[string]any{}
	}

	for _, msgID := range req.Destroy {
		if s.onDestroyEmail != nil {
			if err := s.onDestroyEmail(msgID); err != nil {
				notDestroyed[msgID] = errObj("serverFail", err.Error())
				continue
			}
		}
		s.Delete(msgID)
		destroyed = append(destroyed, msgID)
	}

	return map[string]any{
		"accountId":    accountID,
		"oldState":     oldState,
		"newState":     s.State(),
		"created":      created,
		"updated":      updated,
		"destroyed":    destroyed,
		"notCreated":   notCreated,
		"notUpdated":   notUpdated,
		"notDestroyed": notDestroyed,
	}, nil
}

// HandleEmailCopy implements Email/copy. Not supported; returns serverFail for all.
func (s *Store) HandleEmailCopy(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Create map[jmap.ID]json.RawMessage `json:"create"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck
	notCreated := map[jmap.ID]any{}
	for key := range req.Create {
		notCreated[key] = errObj("serverFail", "Email/copy not supported")
	}
	return map[string]any{
		"fromAccountId": accountID,
		"accountId":     accountID,
		"oldState":      s.State(),
		"newState":      s.State(),
		"created":       map[jmap.ID]any{},
		"notCreated":    notCreated,
	}, nil
}

// HandleEmailImport implements Email/import. Not supported; returns serverFail for all.
func (s *Store) HandleEmailImport(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Emails map[jmap.ID]json.RawMessage `json:"emails"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck
	notCreated := map[jmap.ID]any{}
	for key := range req.Emails {
		notCreated[key] = errObj("serverFail", "Email/import not supported")
	}
	return map[string]any{
		"accountId":  accountID,
		"oldState":   s.State(),
		"newState":   s.State(),
		"created":    map[jmap.ID]any{},
		"notCreated": notCreated,
	}, nil
}

// HandleEmailParse implements Email/parse. Not supported; returns serverFail for all.
func (s *Store) HandleEmailParse(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		BlobIDs []jmap.ID `json:"blobIds"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck
	notParsable := map[jmap.ID]any{}
	for _, id := range req.BlobIDs {
		notParsable[id] = errObj("serverFail", "Email/parse not supported")
	}
	return map[string]any{
		"accountId":   accountID,
		"parsed":      map[jmap.ID]any{},
		"notParsable": notParsable,
	}, nil
}
