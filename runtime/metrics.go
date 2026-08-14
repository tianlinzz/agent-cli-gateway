package runtime

// Metrics is the minimal observability contract for the gateway. The API and
// worker layers call these methods to record counts, gauges, and histograms.
// Implementations MUST bound label cardinality — never pass session IDs, run
// IDs, caller IDs, workspace IDs, or free-form error text as label values.
//
// The interface is deliberately transport-agnostic and stdlib-compatible so it
// can be implemented by a lightweight in-process registry (metrics.Registry)
// or a future Prometheus / OpenTelemetry backend without touching call sites.
type Metrics interface {
	// IncCounter adds value to the named counter. Labels are a small, fixed
	// set of bounded key-value pairs.
	IncCounter(name string, value int64, labels map[string]string)

	// SetGauge sets the named gauge to value. Labels follow the same rules as
	// IncCounter.
	SetGauge(name string, value float64, labels map[string]string)

	// ObserveHistogram records a single observation in the named histogram.
	// Labels follow the same rules as IncCounter.
	ObserveHistogram(name string, value float64, labels map[string]string)
}

// NoopMetrics is a Metrics implementation that discards all observations. It
// is the zero-value default when no metrics backend is configured.
type NoopMetrics struct{}

// IncCounter implements Metrics.
func (NoopMetrics) IncCounter(string, int64, map[string]string) {}

// SetGauge implements Metrics.
func (NoopMetrics) SetGauge(string, float64, map[string]string) {}

// ObserveHistogram implements Metrics.
func (NoopMetrics) ObserveHistogram(string, float64, map[string]string) {}
