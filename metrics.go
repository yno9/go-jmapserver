package jmapserver

import (
	"crypto/subtle"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsOptions configures the Prometheus /metrics endpoint.
type MetricsOptions struct {
	// DataDir is the account data root, laid out as <DataDir>/<domain>/<localpart>/...
	DataDir string
	// RelayLabel is a human relay label (e.g. "ap", "mail"), exported on biset_build_info.
	RelayLabel string
	// Version is the build version string, exported on biset_build_info.
	Version string
	// Token, if non-empty, is required as `Authorization: Bearer <token>` to scrape.
	Token string
}

// RegisterMetrics mounts a strict Prometheus /metrics endpoint on mux.
//
// Exposition follows the Prometheus/OpenMetrics standard via client_golang:
// base units only (bytes/seconds), no self-reported `up` (Prometheus synthesizes
// it per target), plus the standard go_* and process_* collectors and the common
// biset_* metrics computed from DataDir at scrape time.
//
// Relay-specific collectors (e.g. SMTP queue depth, ActivityPub signature errors)
// are passed via extra and registered into the same registry — the common core
// stays here, the per-relay specifics live in each relay.
func RegisterMetrics(mux *http.ServeMux, opts MetricsOptions, extra ...prometheus.Collector) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		&bisetCollector{opts: opts},
	)
	for _, c := range extra {
		reg.MustRegister(c)
	}
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
	mux.Handle("/metrics", bearerAuth(opts.Token, h))
}

// bearerAuth enforces a static bearer token when token != "".
func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			const prefix = "Bearer "
			got := r.Header.Get("Authorization")
			if !strings.HasPrefix(got, prefix) ||
				subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

var (
	descBuildInfo = prometheus.NewDesc(
		"biset_build_info",
		"Build and relay information; the metric value is always 1.",
		[]string{"relay", "version"}, nil,
	)
	descAccounts = prometheus.NewDesc(
		"biset_accounts",
		"Number of provisioned accounts, by domain.",
		[]string{"domain"}, nil,
	)
	descDiskBytes = prometheus.NewDesc(
		"biset_data_disk_bytes",
		"Total size of the data directory tree in bytes.",
		nil, nil,
	)
)

// bisetCollector computes relay state from the data directory at scrape time.
type bisetCollector struct{ opts MetricsOptions }

func (c *bisetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descBuildInfo
	ch <- descAccounts
	ch <- descDiskBytes
}

func (c *bisetCollector) Collect(ch chan<- prometheus.Metric) {
	o := c.opts

	ch <- prometheus.MustNewConstMetric(
		descBuildInfo, prometheus.GaugeValue, 1, o.RelayLabel, o.Version,
	)

	// Accounts per domain: <DataDir>/<domain>/<localpart>.
	if domains, err := os.ReadDir(o.DataDir); err == nil {
		for _, d := range domains {
			if !d.IsDir() {
				continue
			}
			accts, err := os.ReadDir(filepath.Join(o.DataDir, d.Name()))
			if err != nil {
				continue
			}
			n := 0
			for _, a := range accts {
				if a.IsDir() {
					n++
				}
			}
			ch <- prometheus.MustNewConstMetric(
				descAccounts, prometheus.GaugeValue, float64(n), d.Name(),
			)
		}
	}

	// Total disk usage of the data tree.
	var diskBytes int64
	filepath.WalkDir(o.DataDir, func(_ string, e fs.DirEntry, err error) error { //nolint:errcheck
		if err == nil && !e.IsDir() {
			if info, ierr := e.Info(); ierr == nil {
				diskBytes += info.Size()
			}
		}
		return nil
	})
	ch <- prometheus.MustNewConstMetric(descDiskBytes, prometheus.GaugeValue, float64(diskBytes))
}
