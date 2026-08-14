package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// WritePrometheus writes all registered metrics in Prometheus text exposition
// format to w. Metric families are emitted in alphabetical order; samples
// within a family are sorted by label key for deterministic output.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.RLock()
	names := make([]string, 0, len(r.metrics))
	for name := range r.metrics {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)

	for _, name := range names {
		r.mu.RLock()
		mf := r.metrics[name]
		r.mu.RUnlock()
		if mf == nil {
			continue
		}
		if err := writeFamily(w, name, mf); err != nil {
			return err
		}
	}
	return nil
}

func writeFamily(w io.Writer, name string, mf *metricFamily) error {
	switch mf.mt {
	case typeCounter:
		if _, err := fmt.Fprintf(w, "# TYPE %s counter\n", name); err != nil {
			return err
		}
	case typeGauge:
		if _, err := fmt.Fprintf(w, "# TYPE %s gauge\n", name); err != nil {
			return err
		}
	case typeHistogram:
		if _, err := fmt.Fprintf(w, "# TYPE %s histogram\n", name); err != nil {
			return err
		}
	}

	mf.mu.Lock()
	keys := make([]string, 0, len(mf.samples))
	for k := range mf.samples {
		keys = append(keys, k)
	}
	mf.mu.Unlock()
	sort.Strings(keys)

	for _, k := range keys {
		mf.mu.Lock()
		s := mf.samples[k]
		mf.mu.Unlock()
		if err := writeSample(w, name, mf, s); err != nil {
			return err
		}
	}
	return nil
}

func writeSample(w io.Writer, name string, mf *metricFamily, s *sample) error {
	labelStr := formatLabels(s.labelPairs)
	switch mf.mt {
	case typeCounter:
		_, err := fmt.Fprintf(w, "%s%s %d\n", name, labelStr, s.counter.Load())
		return err
	case typeGauge:
		val := math.Float64frombits(s.gauge.Load())
		_, err := fmt.Fprintf(w, "%s%s %s\n", name, labelStr, formatFloat(val))
		return err
	case typeHistogram:
		s.histMu.Lock()
		count := s.count
		sum := s.sum
		bkts := append([]uint64(nil), s.bkts...)
		s.histMu.Unlock()
		bounds := mf.buckets
		for i, bound := range bounds {
			le := formatFloat(bound)
			lb := withExtraLabel(s.labelPairs, "le", le)
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(lb), bkts[i]); err != nil {
				return err
			}
		}
		// +Inf bucket
		lb := withExtraLabel(s.labelPairs, "le", "+Inf")
		if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(lb), count); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_sum%s %s\n", name, labelStr, formatFloat(sum)); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, "%s_count%s %d\n", name, labelStr, count)
		return err
	}
	return nil
}

// formatLabels renders label pairs as `{key="value",...}` sorted by key.
// Returns the empty string when there are no labels.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func withExtraLabel(labels map[string]string, key, value string) map[string]string {
	result := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		result[k] = v
	}
	result[key] = value
	return result
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func formatFloat(v float64) string {
	// Use the shortest representation that round-trips.
	return strings.TrimRight(fmt.Sprintf("%.6g", v), "")
}
