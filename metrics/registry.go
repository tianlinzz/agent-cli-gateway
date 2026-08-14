// Package metrics provides a lightweight in-process metrics registry that
// implements runtime.Metrics without any external dependencies. Counters,
// gauges, and histograms are recorded via atomic operations and exposed in
// Prometheus text exposition format via WritePrometheus.
package metrics

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// defaultBuckets are the histogram bucket upper bounds (seconds) used for
// duration observations. They cover sub-millisecond to multi-minute ranges.
var defaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Registry is a thread-safe in-process metrics registry implementing
// runtime.Metrics. Label cardinality is bounded by the caller — never pass
// session IDs, run IDs, caller IDs, or free-form text as label values.
type Registry struct {
	mu      sync.RWMutex
	metrics map[string]*metricFamily
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]*metricFamily)}
}

type metricType int

const (
	typeCounter metricType = iota
	typeGauge
	typeHistogram
)

type metricFamily struct {
	mt      metricType
	mu      sync.Mutex
	samples map[string]*sample
	buckets []float64 // histogram only
}

type sample struct {
	labelPairs map[string]string // for Prometheus output
	// counter
	counter atomic.Int64
	// gauge (stored as float64 bits in an atomic.Uint64)
	gauge atomic.Uint64
	// histogram
	histMu sync.Mutex
	count  int64
	sum    float64
	bkts   []uint64 // per-bucket count, len = len(buckets)
}

// IncCounter implements runtime.Metrics.
func (r *Registry) IncCounter(name string, value int64, labels map[string]string) {
	r.getOrCreate(name, typeCounter, nil).addCounter(value, labelKey(labels), copyLabels(labels))
}

// SetGauge implements runtime.Metrics.
func (r *Registry) SetGauge(name string, value float64, labels map[string]string) {
	r.getOrCreate(name, typeGauge, nil).setGauge(value, labelKey(labels), copyLabels(labels))
}

// ObserveHistogram implements runtime.Metrics.
func (r *Registry) ObserveHistogram(name string, value float64, labels map[string]string) {
	r.getOrCreate(name, typeHistogram, defaultBuckets).observe(value, labelKey(labels), copyLabels(labels))
}

func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	result := make(map[string]string, len(labels))
	for k, v := range labels {
		result[k] = v
	}
	return result
}

func (r *Registry) getOrCreate(name string, mt metricType, buckets []float64) *metricFamily {
	r.mu.RLock()
	mf, ok := r.metrics[name]
	r.mu.RUnlock()
	if ok {
		return mf
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if mf, ok = r.metrics[name]; ok {
		return mf
	}
	mf = &metricFamily{mt: mt, samples: make(map[string]*sample), buckets: buckets}
	r.metrics[name] = mf
	return mf
}

func (mf *metricFamily) addCounter(value int64, key string, labels map[string]string) {
	mf.mu.Lock()
	s, ok := mf.samples[key]
	if !ok {
		s = &sample{labelPairs: labels}
		mf.samples[key] = s
	}
	mf.mu.Unlock()
	s.counter.Add(value)
}

func (mf *metricFamily) setGauge(value float64, key string, labels map[string]string) {
	mf.mu.Lock()
	s, ok := mf.samples[key]
	if !ok {
		s = &sample{labelPairs: labels}
		mf.samples[key] = s
	}
	mf.mu.Unlock()
	s.gauge.Store(math.Float64bits(value))
}

func (mf *metricFamily) observe(value float64, key string, labels map[string]string) {
	mf.mu.Lock()
	s, ok := mf.samples[key]
	if !ok {
		s = &sample{labelPairs: labels, bkts: make([]uint64, len(mf.buckets))}
		mf.samples[key] = s
	}
	buckets := mf.buckets
	mf.mu.Unlock()
	s.histMu.Lock()
	s.count++
	s.sum += value
	for i, bound := range buckets {
		if value <= bound {
			s.bkts[i]++
		}
	}
	s.histMu.Unlock()
}

// labelKey produces a canonical, sorted string from label key-value pairs.
// Empty label maps produce the empty string.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, labels[k]...)
	}
	return string(b)
}

// Compile-time assertion that Registry implements runtime.Metrics.
var _ runtime.Metrics = (*Registry)(nil)
