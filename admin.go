package jmapserver

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed static/admin_dashboard.html
var adminDashboardHTML embed.FS

// AdminOptions configures the /admin HTTP surface. It mirrors MetricsOptions:
// the same DataDir layout and the same optional bearer Token, but a separate
// token because the admin API exposes per-account message metadata that the
// Prometheus scrape does not.
type AdminOptions struct {
	// DataDir is the account data root, laid out as <DataDir>/<domain>/<localpart>/...
	DataDir string
	// RelayLabel identifies which relay answered (e.g. "ap", "mail").
	RelayLabel string
	// Version is the build version string.
	Version string
	// Token, if non-empty, is required as `Authorization: Bearer <token>`.
	Token string
}

// accountSummary is one row of GET /admin/accounts.
type accountSummary struct {
	Address      string     `json:"address"`
	Domain       string     `json:"domain"`
	Localpart    string     `json:"localpart"`
	Messages     int        `json:"messages"`
	Bytes        int64      `json:"bytes"`
	LastActivity *time.Time `json:"lastActivity,omitempty"`
}

// accountDetail is GET /admin/accounts/{address}: a summary plus a usage
// breakdown and recent activity.
type accountDetail struct {
	accountSummary
	Usage    map[string]int64 `json:"usage"`    // component → bytes
	Activity []ActivityEvent  `json:"activity"` // newest first
}

// RegisterAdmin mounts the admin surface on mux:
//
//	GET /admin/dashboard               — static single-page UI, same-origin only
//	                                      (see static/admin_dashboard.html: it
//	                                      never receives cross-origin data, so
//	                                      the JSON endpoints below keep their
//	                                      existing same-origin-only CORS posture)
//	GET /admin/accounts               — list every provisioned address
//	GET /admin/accounts/{address}     — one address's counts, usage, activity
//
// The JSON endpoints are guarded by opts.Token via the same bearerAuth used for
// /metrics.
func RegisterAdmin(mux *http.ServeMux, opts AdminOptions) {
	mux.HandleFunc("/admin/dashboard", func(w http.ResponseWriter, r *http.Request) {
		b, err := adminDashboardHTML.ReadFile("static/admin_dashboard.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b) //nolint:errcheck
	})

	mux.Handle("/admin/accounts", bearerAuth(opts.Token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{
			"relay":    opts.RelayLabel,
			"version":  opts.Version,
			"accounts": listAccounts(opts.DataDir),
		})
	})))

	mux.Handle("/admin/accounts/", bearerAuth(opts.Token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		addr := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
		lastAt := strings.LastIndex(addr, "@")
		if lastAt <= 0 || lastAt == len(addr)-1 {
			http.Error(w, "bad address", http.StatusBadRequest)
			return
		}
		localpart, domain := addr[:lastAt], addr[lastAt+1:]
		if _, err := os.Stat(filepath.Join(opts.DataDir, domain, localpart)); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		limit := 100
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 {
				limit = n
			}
		}
		writeJSON(w, accountDetailFor(opts, domain, localpart, limit))
	})))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// listAccounts walks <DataDir>/<domain>/<localpart>, the same layout the metrics
// collector uses, and summarizes each account.
func listAccounts(dataDir string) []accountSummary {
	out := []accountSummary{}
	domains, err := os.ReadDir(dataDir)
	if err != nil {
		return out
	}
	for _, d := range domains {
		if !d.IsDir() {
			continue
		}
		domain := d.Name()
		accts, err := os.ReadDir(filepath.Join(dataDir, domain))
		if err != nil {
			continue
		}
		for _, a := range accts {
			if !a.IsDir() {
				continue
			}
			out = append(out, accountSummaryFor(dataDir, domain, a.Name()))
		}
	}
	return out
}

func accountSummaryFor(dataDir, domain, localpart string) accountSummary {
	msgs, msgBytes := messageStats(dataDir, domain, localpart)
	s := accountSummary{
		Address:   localpart + "@" + domain,
		Domain:    domain,
		Localpart: localpart,
		Messages:  msgs,
		Bytes:     msgBytes,
	}
	if evs, err := ReadActivity(dataDir, domain, localpart, 1); err == nil && len(evs) > 0 {
		t := evs[0].Time
		s.LastActivity = &t
	}
	return s
}

func accountDetailFor(opts AdminOptions, domain, localpart string, limit int) accountDetail {
	acctDir := filepath.Join(opts.DataDir, domain, localpart)
	_, msgBytes := messageStats(opts.DataDir, domain, localpart)

	// Usage breakdown: messages (the bulk) plus the whole account subtree, so the
	// difference accounts for avatars, envelopes, keys, and the activity log.
	usage := map[string]int64{"messages": msgBytes, "total": dirBytes(acctDir)}

	activity, _ := ReadActivity(opts.DataDir, domain, localpart, limit)
	if activity == nil {
		activity = []ActivityEvent{}
	}
	return accountDetail{
		accountSummary: accountSummaryFor(opts.DataDir, domain, localpart),
		Usage:          usage,
		Activity:       activity,
	}
}

// messageStats counts the account's persisted message objects and their total
// bytes: <dataDir>/<domain>/<localpart>/messages/*.json (see store.go layout).
func messageStats(dataDir, domain, localpart string) (count int, bytes int64) {
	msgDir := filepath.Join(dataDir, domain, localpart, "messages")
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		count++
		if info, err := e.Info(); err == nil {
			bytes += info.Size()
		}
	}
	return count, bytes
}

// dirBytes sums the sizes of every regular file under root.
func dirBytes(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(_ string, e fs.DirEntry, err error) error { //nolint:errcheck
		if err == nil && !e.IsDir() {
			if info, ierr := e.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
