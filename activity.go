package jmapserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ActivityEvent is one line in an account's activity log — a metadata-only
// record of a message or federation event. Bodies are never recorded: only the
// peer, kind, size, and result, so the log is safe to expose over the admin API.
type ActivityEvent struct {
	Time   time.Time `json:"t"`
	Dir    string    `json:"dir"`              // "in" | "out"
	Kind   string    `json:"kind"`             // "note","follow","unfollow","accept","email",...
	Peer   string    `json:"peer,omitempty"`   // remote handle / address
	MsgID  string    `json:"msgid,omitempty"`  // logical message id, when known
	Bytes  int64     `json:"bytes,omitempty"`  // payload size
	Result string    `json:"result,omitempty"` // "ok","failed",...
	Note   string    `json:"note,omitempty"`   // short summary only — never message body
}

// activityLogName is the append-only JSONL file, one per account, holding its
// ActivityEvent history (newest last).
const activityLogName = "activity.log"

// activityRotateBytes is the size past which the log is rotated to
// activity.log.1 (single generation) before the next append, bounding growth.
const activityRotateBytes = 2 << 20 // 2 MiB

func activityLogPath(dataDir, domain, localpart string) string {
	return filepath.Join(dataDir, domain, localpart, activityLogName)
}

// AppendActivity records ev to the account's activity log. It is best-effort:
// callers log and continue on error rather than failing the underlying message
// operation for the sake of an audit line. A zero ev.Time is stamped now (UTC).
func AppendActivity(dataDir, domain, localpart string, ev ActivityEvent) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	path := activityLogPath(dataDir, domain, localpart)

	// Rotate before appending if the current log has grown past the cap. The
	// account directory already exists (it was provisioned); if it doesn't the
	// open below fails and the caller treats it as best-effort.
	if fi, err := os.Stat(path); err == nil && fi.Size() >= activityRotateBytes {
		os.Rename(path, path+".1") //nolint:errcheck
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// ReadActivity returns up to limit of the account's most recent events, newest
// first. A missing log yields an empty slice and no error (an account that has
// simply never had activity). limit <= 0 defaults to 100.
func ReadActivity(dataDir, domain, localpart string, limit int) ([]ActivityEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	b, err := os.ReadFile(activityLogPath(dataDir, domain, localpart))
	if err != nil {
		if os.IsNotExist(err) {
			return []ActivityEvent{}, nil
		}
		return nil, err
	}

	// Parse all lines, then take the tail. The file is size-bounded by rotation,
	// so a full parse is cheap; scanning from the end would complicate handling
	// of partial/blank lines for little gain at this scale.
	var all []ActivityEvent
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ev ActivityEvent
		if json.Unmarshal(raw, &ev) == nil {
			all = append(all, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Newest first, capped at limit.
	out := make([]ActivityEvent, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out, nil
}
