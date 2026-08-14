package metrics

import (
	"strings"
	"testing"
)

func TestRegistryCounter(t *testing.T) {
	r := NewRegistry()
	r.IncCounter("test_total", 1, map[string]string{"kind": "a"})
	r.IncCounter("test_total", 2, map[string]string{"kind": "a"})
	r.IncCounter("test_total", 5, map[string]string{"kind": "b"})

	var sb strings.Builder
	if err := r.WritePrometheus(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "# TYPE test_total counter") {
		t.Errorf("missing counter type header:\n%s", out)
	}
	if !strings.Contains(out, `test_total{kind="a"} 3`) {
		t.Errorf("missing counter a=3:\n%s", out)
	}
	if !strings.Contains(out, `test_total{kind="b"} 5`) {
		t.Errorf("missing counter b=5:\n%s", out)
	}
}

func TestRegistryGauge(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("active", 42, map[string]string{"adapter": "codex"})
	r.SetGauge("active", 37, map[string]string{"adapter": "codex"})

	var sb strings.Builder
	r.WritePrometheus(&sb)
	out := sb.String()
	if !strings.Contains(out, "# TYPE active gauge") {
		t.Errorf("missing gauge type:\n%s", out)
	}
	if !strings.Contains(out, `active{adapter="codex"} 37`) {
		t.Errorf("gauge not updated to 37:\n%s", out)
	}
}

func TestRegistryHistogram(t *testing.T) {
	r := NewRegistry()
	for _, v := range []float64{0.001, 0.05, 0.3, 2, 15} {
		r.ObserveHistogram("duration", v, map[string]string{"adapter": "codex"})
	}

	var sb strings.Builder
	r.WritePrometheus(&sb)
	out := sb.String()
	if !strings.Contains(out, "# TYPE duration histogram") {
		t.Errorf("missing histogram type:\n%s", out)
	}
	// 5 observations
	if !strings.Contains(out, `duration_count{adapter="codex"} 5`) {
		t.Errorf("missing count=5:\n%s", out)
	}
	// le="0.005" bucket should have 1 (0.001)
	if !strings.Contains(out, `duration_bucket{adapter="codex",le="0.005"} 1`) {
		t.Errorf("missing bucket le=0.005:\n%s", out)
	}
	// le="+Inf" bucket should have 5
	if !strings.Contains(out, `duration_bucket{adapter="codex",le="+Inf"} 5`) {
		t.Errorf("missing bucket le=+Inf:\n%s", out)
	}
}

func TestRegistryNoLabels(t *testing.T) {
	r := NewRegistry()
	r.IncCounter("bare", 1, nil)
	var sb strings.Builder
	r.WritePrometheus(&sb)
	out := sb.String()
	if !strings.Contains(out, "bare 1\n") {
		t.Errorf("bare counter missing:\n%s", out)
	}
}

func TestRegistryImplementsRuntimeMetrics(t *testing.T) {
	var _ interface {
		IncCounter(string, int64, map[string]string)
		SetGauge(string, float64, map[string]string)
		ObserveHistogram(string, float64, map[string]string)
	} = (*Registry)(nil)
}

func TestLabelKeyCanonical(t *testing.T) {
	a := labelKey(map[string]string{"b": "2", "a": "1"})
	b := labelKey(map[string]string{"a": "1", "b": "2"})
	if a != b {
		t.Fatalf("label keys differ despite same content: %q vs %q", a, b)
	}
}
