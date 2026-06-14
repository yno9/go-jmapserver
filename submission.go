package jmapserver

import (
	"encoding/json"
	"fmt"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
)

// HandleEmailSubmissionGet implements EmailSubmission/get. Returns empty list.
func (s *Store) HandleEmailSubmissionGet(accountID jmap.ID, args json.RawMessage) (any, error) {
	return map[string]any{
		"accountId": accountID,
		"state":     "0",
		"list":      []any{},
		"notFound":  []jmap.ID{},
	}, nil
}

// HandleEmailSubmissionChanges implements EmailSubmission/changes. Returns empty changes.
func (s *Store) HandleEmailSubmissionChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
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

// HandleEmailSubmissionQuery implements EmailSubmission/query. Returns empty result.
func (s *Store) HandleEmailSubmissionQuery(accountID jmap.ID, args json.RawMessage) (any, error) {
	return map[string]any{
		"accountId":           accountID,
		"queryState":          "0",
		"canCalculateChanges": false,
		"position":            0,
		"ids":                 []jmap.ID{},
		"total":               0,
	}, nil
}

// HandleEmailSubmissionQueryChanges implements EmailSubmission/queryChanges. Returns empty changes.
func (s *Store) HandleEmailSubmissionQueryChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
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

// HandleEmailSubmissionSet implements EmailSubmission/set.
// For each create: resolves the Email from store/pending, calls OnSubmitEmail hook.
// If hook is not set, returns serverFail.
func (s *Store) HandleEmailSubmissionSet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Create map[jmap.ID]struct {
			EmailID  jmap.ID                   `json:"emailId"`
			Envelope *emailsubmission.Envelope `json:"envelope"`
		} `json:"create"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	created := map[jmap.ID]any{}
	notCreated := map[jmap.ID]any{}

	for key, sub := range req.Create {
		if s.onSubmit == nil {
			notCreated[key] = errObj("serverFail", "EmailSubmission/set not configured")
			continue
		}
		msg, ok := s.TakePending(sub.EmailID)
		if !ok {
			msg, ok = s.Get(sub.EmailID)
		}
		if !ok {
			notCreated[key] = errObj("notFound", fmt.Sprintf("email %q not found", sub.EmailID))
			continue
		}
		env := emailsubmission.Envelope{}
		if sub.Envelope != nil {
			env = *sub.Envelope
		}
		if err := s.onSubmit(msg, env); err != nil {
			notCreated[key] = errObj("serverFail", err.Error())
			continue
		}
		created[key] = map[string]any{
			"id":         "sub-" + string(key),
			"sendAt":     timeNow(),
			"undoStatus": "final",
		}
	}

	return map[string]any{
		"accountId":    accountID,
		"oldState":     "0",
		"newState":     "1",
		"created":      created,
		"notCreated":   notCreated,
		"updated":      map[jmap.ID]any{},
		"notUpdated":   map[jmap.ID]any{},
		"destroyed":    []string{},
		"notDestroyed": map[jmap.ID]any{},
	}, nil
}
