package jmapserver

import (
	"encoding/json"
	"strconv"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

// HandleIdentityGet implements Identity/get.
// Returns stored identities, falling back to a default derived from accountID.
func (s *Store) HandleIdentityGet(accountID jmap.ID) (any, error) {
	s.mu.RLock()
	ids := s.identities
	state := strconv.FormatInt(s.identityState, 10)
	s.mu.RUnlock()

	var list []any
	if len(ids) > 0 {
		for _, id := range ids {
			list = append(list, id)
		}
	} else {
		list = []any{s.defaultIdentity(accountID)}
	}
	return map[string]any{
		"accountId": accountID,
		"state":     state,
		"list":      list,
		"notFound":  []jmap.ID{},
	}, nil
}

// HandleIdentityChanges implements Identity/changes.
func (s *Store) HandleIdentityChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		SinceState string `json:"sinceState"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck
	s.mu.RLock()
	cur := strconv.FormatInt(s.identityState, 10)
	s.mu.RUnlock()
	return map[string]any{
		"accountId":      accountID,
		"oldState":       req.SinceState,
		"newState":       cur,
		"hasMoreChanges": false,
		"created":        []jmap.ID{},
		"updated":        []jmap.ID{},
		"destroyed":      []jmap.ID{},
	}, nil
}

// HandleIdentitySet implements Identity/set: create, update, and destroy identities.
// If OnSetIdentity hook is set, it is called for each operation; a returned error rejects that entry.
func (s *Store) HandleIdentitySet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Create  map[jmap.ID]json.RawMessage `json:"create"`
		Update  map[jmap.ID]json.RawMessage `json:"update"`
		Destroy []jmap.ID                   `json:"destroy"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	s.mu.Lock()
	oldState := strconv.FormatInt(s.identityState, 10)

	// build index by id
	byID := make(map[string]int, len(s.identities))
	for i, id := range s.identities {
		if v, ok := id["id"].(string); ok {
			byID[v] = i
		}
	}

	created := map[jmap.ID]any{}
	notCreated := map[jmap.ID]any{}
	updated := map[jmap.ID]any{}
	notUpdated := map[jmap.ID]any{}
	destroyed := []jmap.ID{}
	notDestroyed := map[jmap.ID]any{}

	for key, raw := range req.Create {
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			notCreated[key] = errObj("invalidProperties", err.Error())
			continue
		}
		newID := "identity-" + string(key)
		if v, ok := data["id"].(string); ok && v != "" {
			newID = v
		}
		data["id"] = newID
		if s.onSetIdentity != nil {
			if err := s.onSetIdentity("create", jmap.ID(newID), data); err != nil {
				notCreated[key] = errObj("serverFail", err.Error())
				continue
			}
		}
		s.identities = append(s.identities, data)
		byID[newID] = len(s.identities) - 1
		s.identityState++
		created[key] = map[string]any{"id": newID}
	}

	for idKey, raw := range req.Update {
		idx, ok := byID[string(idKey)]
		var base map[string]any
		if ok {
			base = s.identities[idx]
		} else if string(idKey) == "identity-"+string(accountID) {
			// Identity/get synthesizes this default on the fly whenever the
			// account has no real identity yet (see defaultIdentity) — it's
			// never actually stored. A client editing "its identity" (e.g.
			// changing the display name) naturally targets this id, since
			// that's the id Identity/get just handed it. Without this,
			// every such update looks like it works (JMAP call succeeds,
			// just lands in notUpdated) but silently does nothing, and the
			// next Identity/get re-synthesizes the untouched original —
			// upsert instead of rejecting as notFound.
			base = s.defaultIdentity(accountID)
		} else {
			notUpdated[idKey] = errObj("notFound", "identity not found")
			continue
		}
		var patch map[string]any
		if err := json.Unmarshal(raw, &patch); err != nil {
			notUpdated[idKey] = errObj("invalidProperties", err.Error())
			continue
		}
		merged := make(map[string]any, len(base))
		for k, v := range base {
			merged[k] = v
		}
		for k, v := range patch {
			merged[k] = v
		}
		if s.onSetIdentity != nil {
			op := "update"
			if !ok {
				op = "create"
			}
			if err := s.onSetIdentity(op, idKey, merged); err != nil {
				notUpdated[idKey] = errObj("serverFail", err.Error())
				continue
			}
		}
		if ok {
			s.identities[idx] = merged
		} else {
			s.identities = append(s.identities, merged)
			byID[string(idKey)] = len(s.identities) - 1
		}
		s.identityState++
		updated[idKey] = map[string]any{}
	}

	destroySet := map[string]bool{}
	for _, idKey := range req.Destroy {
		if _, ok := byID[string(idKey)]; !ok {
			notDestroyed[idKey] = errObj("notFound", "identity not found")
			continue
		}
		if s.onSetIdentity != nil {
			if err := s.onSetIdentity("destroy", idKey, nil); err != nil {
				notDestroyed[idKey] = errObj("serverFail", err.Error())
				continue
			}
		}
		destroySet[string(idKey)] = true
		destroyed = append(destroyed, idKey)
		s.identityState++
	}

	if len(destroySet) > 0 {
		next := s.identities[:0]
		for _, id := range s.identities {
			if v, ok := id["id"].(string); !ok || !destroySet[v] {
				next = append(next, id)
			}
		}
		s.identities = next
	}

	if len(req.Create) > 0 || len(req.Update) > 0 || len(destroySet) > 0 {
		s.saveIdentitiesLocked()
	}

	newState := strconv.FormatInt(s.identityState, 10)
	s.mu.Unlock()

	return map[string]any{
		"accountId":    accountID,
		"oldState":     oldState,
		"newState":     newState,
		"created":      created,
		"updated":      updated,
		"destroyed":    destroyed,
		"notCreated":   notCreated,
		"notUpdated":   notUpdated,
		"notDestroyed": notDestroyed,
	}, nil
}
