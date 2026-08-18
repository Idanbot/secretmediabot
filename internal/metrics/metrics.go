// Package metrics implements a minimal, dependency-free metrics registry
// with a Prometheus text exposition format handler. It deliberately supports
// only counters and gauges with a fixed, low-cardinality label set, which is
// all the application needs; histogram-style observations are approximated
// with counted buckets where required.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type child struct {
	rendered string
	value    atomic.Int64
}

type metric struct {
	name       string
	help       string
	labelNames []string
	metricType string
	children   sync.Map // rendered label key -> *child
}

func (m *metric) childFor(labelValues []string) *child {
	if len(labelValues) != len(m.labelNames) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", m.name, len(m.labelNames), len(labelValues)))
	}
	key := strings.Join(labelValues, "\x00")
	if existing, ok := m.children.Load(key); ok {
		return existing.(*child)
	}
	rendered := ""
	if len(labelValues) > 0 {
		pairs := make([]string, 0, len(labelValues))
		for i, name := range m.labelNames {
			pairs = append(pairs, fmt.Sprintf("%s=%q", name, escapeLabel(labelValues[i])))
		}
		rendered = "{" + strings.Join(pairs, ",") + "}"
	}
	created := &child{rendered: rendered}
	actual, loaded := m.children.LoadOrStore(key, created)
	_ = loaded
	return actual.(*child)
}

func escapeLabel(value string) string {
	replacer := strings.NewReplacer("\\", `\\`, "\n", `\n`, `"`, `\"`)
	return replacer.Replace(value)
}

func (m *metric) writeTo(builder *strings.Builder) {
	fmt.Fprintf(builder, "# HELP %s %s\n", m.name, m.help)
	fmt.Fprintf(builder, "# TYPE %s %s\n", m.name, m.metricType)
	type entry struct {
		rendered string
		value    int64
	}
	entries := make([]entry, 0, 8)
	m.children.Range(func(_, value any) bool {
		c := value.(*child)
		entries = append(entries, entry{rendered: c.rendered, value: c.value.Load()})
		return true
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].rendered < entries[j].rendered })
	for _, e := range entries {
		fmt.Fprintf(builder, "%s%s %d\n", m.name, e.rendered, e.value)
	}
}

// CounterMetric is a monotonically increasing counter.
type CounterMetric struct{ *metric }

// Inc adds one for the given label values.
func (c *CounterMetric) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add accumulates delta for the given label values.
func (c *CounterMetric) Add(delta int64, labelValues ...string) {
	c.childFor(labelValues).value.Add(delta)
}

// GaugeMetric reports a point-in-time value.
type GaugeMetric struct{ *metric }

// Set stores the current value for the given label values.
func (g *GaugeMetric) Set(value int64, labelValues ...string) {
	ch := g.childFor(labelValues)
	for {
		current := ch.value.Load()
		if ch.value.CompareAndSwap(current, value) {
			return
		}
	}
}

// Add adjusts the gauge by delta.
func (g *GaugeMetric) Add(delta int64, labelValues ...string) {
	g.childFor(labelValues).value.Add(delta)
}

type registry struct {
	mu      sync.Mutex
	metrics []*metric
	byName  map[string]*metric
}

var defaultRegistry = &registry{byName: make(map[string]*metric)}

func (r *registry) lookup(name, help, metricType string, labelNames []string) *metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byName[name]; ok {
		if existing.help != help || existing.metricType != metricType || strings.Join(existing.labelNames, ",") != strings.Join(labelNames, ",") {
			panic(fmt.Sprintf("metrics: %s already registered with a different signature", name))
		}
		return existing
	}
	created := &metric{name: name, help: help, labelNames: labelNames, metricType: metricType}
	r.byName[name] = created
	r.metrics = append(r.metrics, created)
	return created
}

// Counter registers (once) and returns a counter. Calls are safe from
// package-init time and concurrent goroutines.
func Counter(name, help string, labelNames ...string) *CounterMetric {
	return &CounterMetric{defaultRegistry.lookup(name, help, "counter", labelNames)}
}

// Gauge registers (once) and returns a gauge.
func Gauge(name, help string, labelNames ...string) *GaugeMetric {
	return &GaugeMetric{defaultRegistry.lookup(name, help, "gauge", labelNames)}
}

// Snapshot renders the registry in the Prometheus text exposition format.
func Snapshot() string {
	defaultRegistry.mu.Lock()
	metrics := append([]*metric(nil), defaultRegistry.metrics...)
	defaultRegistry.mu.Unlock()
	var builder strings.Builder
	for _, m := range metrics {
		m.writeTo(&builder)
	}
	return builder.String()
}

// Handler serves Snapshot as text/plain.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(Snapshot()))
	})
}
