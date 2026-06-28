package jmapserver

import (
	"encoding/json"
	"fmt"
	"strconv"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
)

// HandleEmailSubmissionGet implements EmailSubmission/get. Returns persisted submissions.
func (s *Store) HandleEmailSubmissionGet(accountID jmap.ID, args json.RawMessage) (any, error) {
	subs := s.Submissions()
	list := make([]any, len(subs))
	for i, sub := range subs {
		list[i] = sub
	}
	return map[string]any{
		"accountId": accountID,
		"state":     s.SubmissionState(),
		"list":      list,
		"notFound":  []jmap.ID{},
	}, nil
}

// HandleEmailSubmissionChanges implements EmailSubmission/changes.
func (s *Store) HandleEmailSubmissionChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		SinceState string `json:"sinceState"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	s.mu.RLock()
	curState := s.submissionState
	s.mu.RUnlock()

	// We don't track per-version changes for submissions; if state changed,
	// report cannotCalculateChanges so client re-fetches via EmailSubmission/get.
	since, err := strconv.ParseInt(req.SinceState, 10, 64)
	if err != nil || since < 0 || since > curState {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}

	// No per-item tracking; return empty changes (client must use EmailSubmission/get for details)
	return map[string]any{
		"accountId":      accountID,
		"oldState":       req.SinceState,
		"newState":       strconv.FormatInt(curState, 10),
		"hasMoreChanges": false,
		"created":        []jmap.ID{},
		"updated":        []jmap.ID{},
		"destroyed":      []jmap.ID{},
	}, nil
}

// HandleEmailSubmissionQuery implements EmailSubmission/query. Returns submission IDs.
func (s *Store) HandleEmailSubmissionQuery(accountID jmap.ID, args json.RawMessage) (any, error) {
	subs := s.Submissions()
	ids := make([]jmap.ID, 0, len(subs))
	for _, sub := range subs {
		if id, ok := sub["id"].(string); ok {
			ids = append(ids, jmap.ID(id))
		}
	}
	return map[string]any{
		"accountId":           accountID,
		"queryState":          s.SubmissionState(),
		"canCalculateChanges": false,
		"position":            0,
		"ids":                 ids,
		"total":               len(ids),
	}, nil
}

// HandleEmailSubmissionQueryChanges implements EmailSubmission/queryChanges.
func (s *Store) HandleEmailSubmissionQueryChanges(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		SinceQueryState string `json:"sinceQueryState"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	s.mu.RLock()
	cur := s.submissionState
	s.mu.RUnlock()

	since, err := strconv.ParseInt(req.SinceQueryState, 10, 64)
	if err != nil || since < 0 || since > cur {
		return nil, fmt.Errorf("cannotCalculateChanges")
	}

	return map[string]any{
		"accountId":     accountID,
		"oldQueryState": req.SinceQueryState,
		"newQueryState": strconv.FormatInt(cur, 10),
		"removed":       []jmap.ID{},
		"added":         []map[string]any{},
	}, nil
}

// HandleEmailSubmissionSet implements EmailSubmission/set.
// For each create: resolves the Email, calls OnSubmitEmail hook, persists the submission record.
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
		subID := "sub-" + string(key) + "-" + timeNow()
		rec := map[string]any{
			"id":         subID,
			"identityId": "",
			"emailId":    string(msg.ID),
			"threadId":   string(msg.ThreadID),
			"sendAt":     timeNow(),
			"undoStatus": "final",
		}
		s.AddSubmission(rec)
		created[key] = map[string]any{
			"id":         subID,
			"sendAt":     rec["sendAt"],
			"undoStatus": "final",
		}
	}

	return map[string]any{
		"accountId":    accountID,
		"oldState":     s.SubmissionState(),
		"newState":     s.SubmissionState(),
		"created":      created,
		"notCreated":   notCreated,
		"updated":      map[jmap.ID]any{},
		"notUpdated":   map[jmap.ID]any{},
		"destroyed":    []string{},
		"notDestroyed": map[jmap.ID]any{},
	}, nil
}
