package jmapserver

import (
	"encoding/json"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

// HandleVacationResponseGet implements VacationResponse/get.
func (s *Store) HandleVacationResponseGet(accountID jmap.ID, args json.RawMessage) (any, error) {
	s.mu.RLock()
	vr := s.vacation
	s.mu.RUnlock()
	if vr == nil {
		vr = map[string]any{
			"id":        "singleton",
			"isEnabled": false,
			"subject":   nil,
			"textBody":  nil,
			"htmlBody":  nil,
		}
	}
	return map[string]any{
		"accountId": accountID,
		"state":     "0",
		"list":      []any{vr},
		"notFound":  []jmap.ID{},
	}, nil
}

// HandleVacationResponseSet implements VacationResponse/set.
func (s *Store) HandleVacationResponseSet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Update map[string]json.RawMessage `json:"update"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	updated := map[string]any{}
	notUpdated := map[string]any{}

	for id, rawPatch := range req.Update {
		var patch map[string]any
		if err := json.Unmarshal(rawPatch, &patch); err != nil {
			notUpdated[id] = errObj("invalidProperties", err.Error())
			continue
		}
		s.mu.Lock()
		if s.vacation == nil {
			s.vacation = map[string]any{"id": "singleton", "isEnabled": false}
		}
		for k, v := range patch {
			s.vacation[k] = v
		}
		s.mu.Unlock()
		updated[id] = map[string]any{}
	}

	return map[string]any{
		"accountId":    accountID,
		"oldState":     "0",
		"newState":     "0",
		"created":      map[string]any{},
		"updated":      updated,
		"destroyed":    []string{},
		"notCreated":   map[string]any{},
		"notUpdated":   notUpdated,
		"notDestroyed": map[string]any{},
	}, nil
}
