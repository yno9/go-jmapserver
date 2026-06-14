package jmapserver

import (
	"encoding/json"
	"fmt"
	"strings"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

// Dispatch routes all RFC 8621 JMAP methods to the appropriate Store handler.
// Protocol-specific fetch logic (e.g. IMAP FetchNew before Email/query) must
// run before calling Dispatch, or via the OnCreateEmail / OnSubmitEmail hooks.
func (s *Store) Dispatch(accountID jmap.ID, method string, args json.RawMessage) (any, error) {
	switch method {
	case "Mailbox/get":
		return s.HandleMailboxGet(accountID, args)
	case "Mailbox/changes":
		return s.HandleMailboxChanges(accountID, args)
	case "Mailbox/query":
		return s.HandleMailboxQuery(accountID, args)
	case "Mailbox/queryChanges":
		return s.HandleMailboxQueryChanges(accountID, args)
	case "Mailbox/set":
		return s.HandleMailboxSet(accountID, args)
	case "Thread/get":
		return s.HandleThreadGet(accountID, args)
	case "Thread/changes":
		return s.HandleThreadChanges(accountID, args)
	case "Email/get":
		return s.HandleEmailGet(accountID, args)
	case "Email/changes":
		return s.HandleEmailChanges(accountID, args)
	case "Email/query":
		return s.HandleEmailQuery(accountID, args)
	case "Email/queryChanges":
		return s.HandleQueryChanges(accountID, args)
	case "Email/set":
		return s.HandleEmailSet(accountID, args)
	case "Email/copy":
		return s.HandleEmailCopy(accountID, args)
	case "Email/import":
		return s.HandleEmailImport(accountID, args)
	case "Email/parse":
		return s.HandleEmailParse(accountID, args)
	case "SearchSnippet/get":
		return s.HandleSearchSnippetGet(accountID, args)
	case "Identity/get":
		return s.HandleIdentityGet(accountID)
	case "Identity/changes":
		return s.HandleIdentityChanges(accountID, args)
	case "Identity/set":
		return s.HandleIdentitySet(accountID, args)
	case "EmailSubmission/get":
		return s.HandleEmailSubmissionGet(accountID, args)
	case "EmailSubmission/changes":
		return s.HandleEmailSubmissionChanges(accountID, args)
	case "EmailSubmission/query":
		return s.HandleEmailSubmissionQuery(accountID, args)
	case "EmailSubmission/queryChanges":
		return s.HandleEmailSubmissionQueryChanges(accountID, args)
	case "EmailSubmission/set":
		return s.HandleEmailSubmissionSet(accountID, args)
	case "VacationResponse/get":
		return s.HandleVacationResponseGet(accountID, args)
	case "VacationResponse/set":
		return s.HandleVacationResponseSet(accountID, args)
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

// HandleSearchSnippetGet implements SearchSnippet/get.
// Returns a plain-text snippet for each email ID by searching body text.
func (s *Store) HandleSearchSnippetGet(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Filter   *struct{ Text string `json:"text"` } `json:"filter"`
		EmailIDs []jmap.ID                            `json:"emailIds"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	list := []map[string]any{}
	notFound := []jmap.ID{}
	for _, id := range req.EmailIDs {
		m, ok := s.Get(id)
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		snippet := map[string]any{"emailId": id, "subject": nil, "preview": nil}
		if req.Filter != nil && req.Filter.Text != "" {
			q := strings.ToLower(req.Filter.Text)
			if subj := firstMatchSnippet(m.Subject, q); subj != "" {
				snippet["subject"] = subj
			}
			body := ""
			if len(m.BodyValues) > 0 {
				for _, bv := range m.BodyValues {
					body = bv.Value
					break
				}
			}
			if prev := firstMatchSnippet(body, q); prev != "" {
				snippet["preview"] = prev
			}
		}
		list = append(list, snippet)
	}
	return map[string]any{
		"accountId": accountID,
		"list":      list,
		"notFound":  notFound,
	}, nil
}

func firstMatchSnippet(text, query string) string {
	idx := strings.Index(strings.ToLower(text), query)
	if idx < 0 {
		return ""
	}
	start := idx - 20
	if start < 0 {
		start = 0
	}
	end := idx + len(query) + 80
	if end > len(text) {
		end = len(text)
	}
	return "…" + text[start:end] + "…"
}
