// Package metrics is a small, dependency-free registry of labelled counters and histograms,
// exposed in the Prometheus text format.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

type series struct {
	labels string
	value  float64
	sum    float64
	counts []uint64 // histogram buckets (cumulative on output)
}

type metric struct {
	name, help, kind string
	buckets          []float64
	mu               sync.Mutex
	series           map[string]*series
}

// Registry holds Zirnis's business metrics.
type Registry struct {
	metrics []*metric

	Payments        *Counter
	Payouts         *Counter
	LedgerPostings  *Counter
	RiskDecisions   *Counter
	ReconExceptions *Counter
	ProviderCalls   *Histogram
	HTTPRequests    *Histogram
}

func New() *Registry {
	r := &Registry{}
	r.Payments = r.counter("zirnis_payments_total", "Payment intents by terminal status")
	r.Payouts = r.counter("zirnis_payouts_total", "Payouts by status transition")
	r.LedgerPostings = r.counter("zirnis_ledger_postings_total", "Journal entries posted")
	r.RiskDecisions = r.counter("zirnis_risk_decisions_total", "Risk engine decisions")
	r.ReconExceptions = r.counter("zirnis_reconciliation_exceptions_total", "Reconciliation breaks found")
	latency := []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}
	r.ProviderCalls = r.histogram("zirnis_provider_duration_ms", "Provider call latency in milliseconds", latency)
	r.HTTPRequests = r.histogram("zirnis_http_request_duration_ms", "HTTP request latency in milliseconds", latency)
	return r
}

// Counter is a monotonically increasing, labelled counter.
type Counter struct{ m *metric }

// Histogram records observations into fixed buckets.
type Histogram struct{ m *metric }

func (r *Registry) counter(name, help string) *Counter {
	m := &metric{name: name, help: help, kind: "counter", series: map[string]*series{}}
	r.metrics = append(r.metrics, m)
	return &Counter{m}
}

func (r *Registry) histogram(name, help string, buckets []float64) *Histogram {
	m := &metric{name: name, help: help, kind: "histogram", buckets: buckets, series: map[string]*series{}}
	r.metrics = append(r.metrics, m)
	return &Histogram{m}
}

// labels renders key/value pairs, e.g. Labels("status", "Paid") → status="Paid".
func labels(kv []string) string {
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", kv[i], kv[i+1]))
	}
	return strings.Join(parts, ",")
}

func (m *metric) get(kv []string) *series {
	key := labels(kv)
	s, ok := m.series[key]
	if !ok {
		s = &series{labels: key, counts: make([]uint64, len(m.buckets))}
		m.series[key] = s
	}
	return s
}

// Add increments the counter for the given label pairs.
func (c *Counter) Add(n float64, kv ...string) {
	if c == nil {
		return
	}
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	c.m.get(kv).value += n
}

// Inc increments the counter by one.
func (c *Counter) Inc(kv ...string) { c.Add(1, kv...) }

// Observe records a value.
func (h *Histogram) Observe(v float64, kv ...string) {
	if h == nil {
		return
	}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	s := h.m.get(kv)
	s.value++
	s.sum += v
	for i, b := range h.m.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
}

// Write writes every metric in the Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) {
	for _, m := range r.metrics {
		m.mu.Lock()
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)
		keys := make([]string, 0, len(m.series))
		for k := range m.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := m.series[k]
			if m.kind == "counter" {
				fmt.Fprintf(w, "%s{%s} %g\n", m.name, s.labels, s.value)
				continue
			}
			sep := ""
			if s.labels != "" {
				sep = ","
			}
			for i, b := range m.buckets {
				fmt.Fprintf(w, "%s_bucket{%s%sle=\"%g\"} %d\n", m.name, s.labels, sep, b, s.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %g\n", m.name, s.labels, sep, s.value)
			fmt.Fprintf(w, "%s_sum{%s} %g\n%s_count{%s} %g\n", m.name, s.labels, s.sum, m.name, s.labels, s.value)
		}
		m.mu.Unlock()
	}
}
