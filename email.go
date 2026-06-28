package jmapserver

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"strconv"
	"strings"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
)

// emailMatchesText returns true if the email contains q (case-insensitive)
// in subject, from/to name or email address, or plain-text body.
func emailMatchesText(m email.Email, q string) bool {
	q = strings.ToLower(q)
	if strings.Contains(strings.ToLower(m.Subject), q) {
		return true
	}
	for _, addrs := range [][]*mail.Address{m.From, m.To, m.CC} {
		for _, a := range addrs {
			if a == nil {
				continue
			}
			if strings.Contains(strings.ToLower(a.Name), q) || strings.Contains(strings.ToLower(a.Email), q) {
				return true
			}
		}
	}
	for _, bv := range m.BodyValues {
		if bv != nil && strings.Contains(strings.ToLower(bv.Value), q) {
			return true
		}
	}
	return false
}

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

// HandleEmailQuery implements Email/query with optional filter (inMailbox, text), position, and limit.
func (s *Store) HandleEmailQuery(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Filter   *struct {
			InMailbox string `json:"inMailbox"`
			Text      string `json:"text"`
		} `json:"filter"`
		Position int    `json:"position"`
		Limit    uint64 `json:"limit"`
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
		if req.Filter != nil && req.Filter.Text != "" {
			if !emailMatchesText(m, req.Filter.Text) {
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

// HandleEmailCopy implements Email/copy. Copies an email to a different mailbox.
func (s *Store) HandleEmailCopy(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		FromAccountID jmap.ID `json:"fromAccountId"`
		Create        map[jmap.ID]struct {
			ID         jmap.ID          `json:"id"`
			MailboxIDs map[jmap.ID]bool `json:"mailboxIds"`
		} `json:"create"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	created := map[jmap.ID]any{}
	notCreated := map[jmap.ID]any{}

	for key, spec := range req.Create {
		src, ok := s.Get(spec.ID)
		if !ok {
			notCreated[key] = errObj("notFound", fmt.Sprintf("email %q not found", spec.ID))
			continue
		}
		cp := src
		newID := jmap.ID(string(spec.ID) + "-cp-" + string(key))
		cp.ID = newID
		if len(spec.MailboxIDs) > 0 {
			cp.MailboxIDs = spec.MailboxIDs
		}
		if err := s.Put(cp); err != nil {
			notCreated[key] = errObj("serverFail", err.Error())
			continue
		}
		created[key] = map[string]any{"id": newID}
	}

	fromID := req.FromAccountID
	if fromID == "" {
		fromID = accountID
	}
	return map[string]any{
		"fromAccountId": fromID,
		"accountId":     accountID,
		"oldState":      s.State(),
		"newState":      s.State(),
		"created":       created,
		"notCreated":    notCreated,
	}, nil
}

// HandleEmailImport implements Email/import. Reads a blob, parses as MIME, creates Email.
func (s *Store) HandleEmailImport(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		Emails map[jmap.ID]struct {
			BlobID     jmap.ID          `json:"blobId"`
			MailboxIDs map[jmap.ID]bool `json:"mailboxIds"`
			Keywords   map[string]bool  `json:"keywords"`
			ReceivedAt string           `json:"receivedAt"`
		} `json:"emails"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	created := map[jmap.ID]any{}
	notCreated := map[jmap.ID]any{}

	for key, spec := range req.Emails {
		data, ok := s.GetBlob(string(spec.BlobID))
		if !ok {
			notCreated[key] = errObj("notFound", fmt.Sprintf("blob %q not found", spec.BlobID))
			continue
		}
		m, err := ParseMIMEEmail(data)
		if err != nil {
			notCreated[key] = errObj("invalidEmail", err.Error())
			continue
		}
		m.ID = jmap.ID("import-" + string(key) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36))
		if len(spec.MailboxIDs) > 0 {
			m.MailboxIDs = spec.MailboxIDs
		}
		if len(spec.Keywords) > 0 {
			m.Keywords = spec.Keywords
		}
		if spec.ReceivedAt != "" {
			if t, err2 := time.Parse(time.RFC3339, spec.ReceivedAt); err2 == nil {
				m.ReceivedAt = &t
			}
		}
		if err := s.Put(m); err != nil {
			notCreated[key] = errObj("serverFail", err.Error())
			continue
		}
		created[key] = map[string]any{"id": m.ID}
	}

	return map[string]any{
		"accountId":  accountID,
		"oldState":   s.State(),
		"newState":   s.State(),
		"created":    created,
		"notCreated": notCreated,
	}, nil
}

// HandleEmailParse implements Email/parse. Reads a blob and returns parsed email preview.
func (s *Store) HandleEmailParse(accountID jmap.ID, args json.RawMessage) (any, error) {
	var req struct {
		BlobIDs    []jmap.ID `json:"blobIds"`
		Properties []string  `json:"properties"`
	}
	json.Unmarshal(args, &req) //nolint:errcheck

	parsed := map[jmap.ID]any{}
	notParsable := map[jmap.ID]any{}

	for _, blobID := range req.BlobIDs {
		data, ok := s.GetBlob(string(blobID))
		if !ok {
			notParsable[blobID] = errObj("notFound", fmt.Sprintf("blob %q not found", blobID))
			continue
		}
		m, err := ParseMIMEEmail(data)
		if err != nil {
			notParsable[blobID] = errObj("notParsable", err.Error())
			continue
		}
		parsed[blobID] = m
	}

	return map[string]any{
		"accountId":   accountID,
		"parsed":      parsed,
		"notParsable": notParsable,
	}, nil
}

// ParseMIMEEmail parses raw RFC 5322 bytes into an email.Email struct.
// Subject, From, To, MessageID, InReplyTo, References, and body are decoded
// (quoted-printable, base64, encoded-words, multipart/alternative).
// The caller must set ID, MailboxIDs, and ReceivedAt as appropriate.
func ParseMIMEEmail(data []byte) (email.Email, error) {
	msg, err := stdmail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return email.Email{}, err
	}

	dec := mime.WordDecoder{}
	subject, _ := dec.DecodeHeader(msg.Header.Get("Subject"))
	msgIDRaw := strings.Trim(msg.Header.Get("Message-Id"), " <>")
	inReplyToRaw := strings.Trim(msg.Header.Get("In-Reply-To"), " <>")

	date := time.Now()
	if d, err2 := stdmail.ParseDate(msg.Header.Get("Date")); err2 == nil {
		date = d
	}

	var fromAddrs []*mail.Address
	if a, err2 := stdmail.ParseAddress(msg.Header.Get("From")); err2 == nil {
		fromAddrs = []*mail.Address{{Email: a.Address, Name: a.Name}}
	}
	var toAddrs []*mail.Address
	if addrs, err2 := stdmail.ParseAddressList(msg.Header.Get("To")); err2 == nil {
		for _, a := range addrs {
			toAddrs = append(toAddrs, &mail.Address{Email: a.Address, Name: a.Name})
		}
	}

	bodyText := extractMIMEText(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body)

	var msgIDs, inReplyTos, refs []string
	if msgIDRaw != "" {
		msgIDs = []string{msgIDRaw}
	}
	if inReplyToRaw != "" {
		inReplyTos = []string{inReplyToRaw}
	}
	for _, r := range strings.Fields(msg.Header.Get("References")) {
		if r = strings.Trim(r, "<>"); r != "" {
			refs = append(refs, r)
		}
	}

	partID := "1"
	m := email.Email{
		Subject:    subject,
		From:       fromAddrs,
		To:         toAddrs,
		ReceivedAt: &date,
		MessageID:  msgIDs,
		InReplyTo:  inReplyTos,
		References: refs,
		Keywords:   map[string]bool{},
		MailboxIDs: map[jmap.ID]bool{},
		BodyValues: map[string]*email.BodyValue{
			partID: {Value: bodyText},
		},
		TextBody: []*email.BodyPart{{
			PartID: partID,
			Type:   "text/plain",
		}},
	}
	return m, nil
}

// extractMIMEText extracts the text/plain body from a MIME message.
func extractMIMEText(contentType, contentEncoding string, body io.Reader) string {
	if contentType == "" {
		b, _ := io.ReadAll(body)
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		b, _ := io.ReadAll(body)
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	switch {
	case mediaType == "text/plain":
		b, _ := io.ReadAll(decodeMIMETransfer(contentEncoding, body))
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	case mediaType == "multipart/encrypted":
		// PGP/MIME (RFC 3156): extract the application/octet-stream part containing the PGP block.
		// Skip any text/plain fallback to avoid storing plaintext server-side.
		boundary := params["boundary"]
		if boundary == "" {
			return ""
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err2 := mr.NextPart()
			if err2 != nil {
				break
			}
			partCT := part.Header.Get("Content-Type")
			partMedia, _, _ := mime.ParseMediaType(partCT)
			if partMedia == "application/octet-stream" || partMedia == "application/pgp-encrypted" {
				b, _ := io.ReadAll(decodeMIMETransfer(part.Header.Get("Content-Transfer-Encoding"), part))
				s := strings.ReplaceAll(string(b), "\r\n", "\n")
				if strings.Contains(s, "-----BEGIN PGP MESSAGE-----") {
					return s
				}
			}
		}
		return ""
	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return ""
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err2 := mr.NextPart()
			if err2 != nil {
				break
			}
			partCT := part.Header.Get("Content-Type")
			if partCT == "" {
				continue
			}
			partMedia, _, _ := mime.ParseMediaType(partCT)
			if partMedia == "text/plain" {
				b, _ := io.ReadAll(decodeMIMETransfer(part.Header.Get("Content-Transfer-Encoding"), part))
				return strings.ReplaceAll(string(b), "\r\n", "\n")
			}
			// Recurse into nested multipart (e.g. multipart/mixed containing multipart/encrypted).
			if strings.HasPrefix(partMedia, "multipart/") {
				if nested := extractMIMEText(partCT, part.Header.Get("Content-Transfer-Encoding"), part); nested != "" {
					return nested
				}
			}
		}
	}
	return ""
}

// decodeMIMETransfer wraps r with the appropriate Content-Transfer-Encoding decoder.
func decodeMIMETransfer(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	}
	return r
}

// MessageBody returns the text/plain body of an email.Email.
func MessageBody(m email.Email) string {
	if len(m.TextBody) > 0 && m.TextBody[0] != nil {
		partID := m.TextBody[0].PartID
		if bv, ok := m.BodyValues[partID]; ok && bv != nil {
			return bv.Value
		}
	}
	return ""
}

// BuildRFC5322 serializes an email.Email into RFC 5322 wire format suitable
// for SMTP transmission. Generates a Message-ID if absent (using defaultDomain
// for the right-hand side, or the From address domain). Returns the raw bytes
// and the Message-ID (without angle brackets).
//
// Headers emitted: From, To, Cc, Subject, Date, Message-Id, In-Reply-To, References,
// Content-Type. Body is the first text/plain BodyValue.
func BuildRFC5322(e email.Email, defaultDomain string) ([]byte, string) {
	from := ""
	if len(e.From) > 0 && e.From[0] != nil {
		from = formatAddr(e.From[0])
	}
	to := joinAddrs(e.To)
	cc := joinAddrs(e.CC)

	domain := defaultDomain
	if domain == "" && len(e.From) > 0 && e.From[0] != nil {
		if parts := strings.SplitN(e.From[0].Email, "@", 2); len(parts) == 2 {
			domain = parts[1]
		}
	}
	if domain == "" {
		domain = "localhost"
	}

	msgID := ""
	if len(e.MessageID) > 0 && e.MessageID[0] != "" {
		msgID = strings.Trim(e.MessageID[0], "<>")
	} else {
		rnd := make([]byte, 6)
		_, _ = cryptorand.Read(rnd)
		msgID = fmt.Sprintf("%d.%s@%s", time.Now().UnixNano(), hex.EncodeToString(rnd), domain)
	}

	date := time.Now()
	if e.SentAt != nil {
		date = time.Time(*e.SentAt)
	} else if e.ReceivedAt != nil {
		date = time.Time(*e.ReceivedAt)
	}

	var b strings.Builder
	if from != "" {
		b.WriteString("From: " + from + "\r\n")
	}
	if to != "" {
		b.WriteString("To: " + to + "\r\n")
	}
	if cc != "" {
		b.WriteString("Cc: " + cc + "\r\n")
	}
	b.WriteString("Subject: " + e.Subject + "\r\n")
	b.WriteString("Date: " + date.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-Id: <" + msgID + ">\r\n")
	if len(e.InReplyTo) > 0 {
		b.WriteString("In-Reply-To: " + bracketJoin(e.InReplyTo) + "\r\n")
	}
	if len(e.References) > 0 {
		b.WriteString("References: " + bracketJoin(e.References) + "\r\n")
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(MessageBody(e))
	return []byte(b.String()), msgID
}

func formatAddr(a *mail.Address) string {
	if a == nil || a.Email == "" {
		return ""
	}
	return (&stdmail.Address{Name: a.Name, Address: a.Email}).String()
}

func joinAddrs(addrs []*mail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if s := formatAddr(a); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

func bracketJoin(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.Trim(id, " <>")
		if id != "" {
			parts = append(parts, "<"+id+">")
		}
	}
	return strings.Join(parts, " ")
}

// BuildEnvelope constructs an SMTP envelope from the email's From/To/CC/BCC headers.
// Returns nil if From is absent or no recipients can be found.
func BuildEnvelope(e email.Email) *emailsubmission.Envelope {
	var mailFrom string
	if len(e.From) > 0 && e.From[0] != nil {
		mailFrom = e.From[0].Email
	}
	if mailFrom == "" {
		return nil
	}
	seen := map[string]bool{}
	var rcpt []*emailsubmission.Address
	for _, addrs := range [][]*mail.Address{e.To, e.CC, e.BCC} {
		for _, a := range addrs {
			if a != nil && a.Email != "" && !seen[a.Email] {
				seen[a.Email] = true
				rcpt = append(rcpt, &emailsubmission.Address{Email: a.Email})
			}
		}
	}
	if len(rcpt) == 0 {
		return nil
	}
	return &emailsubmission.Envelope{
		MailFrom: &emailsubmission.Address{Email: mailFrom},
		RcptTo:   rcpt,
	}
}
