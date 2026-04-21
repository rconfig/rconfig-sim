package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// TestNewRegistersAllMetrics asserts every required metric is registered with
// the expected name and type. Regressions in this table mean a metric got
// renamed or dropped — both rcfg-sim operations and any dashboards downstream
// care. Runs without hitting the HTTP handler.
func TestNewRegistersAllMetrics(t *testing.T) {
	reg := New()

	want := map[string]dto.MetricType{
		"rcfgsim_active_sessions":            dto.MetricType_GAUGE,
		"rcfgsim_sessions_total":             dto.MetricType_COUNTER,
		"rcfgsim_session_duration_seconds":   dto.MetricType_HISTOGRAM,
		"rcfgsim_command_duration_seconds":   dto.MetricType_HISTOGRAM,
		"rcfgsim_bytes_sent_total":           dto.MetricType_COUNTER,
		"rcfgsim_auth_attempts_total":        dto.MetricType_COUNTER,
		"rcfgsim_handshake_duration_seconds": dto.MetricType_HISTOGRAM,
		"rcfgsim_faults_injected_total":      dto.MetricType_COUNTER,
	}

	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]dto.MetricType{}
	for _, mf := range mfs {
		got[mf.GetName()] = mf.GetType()
	}
	for name, wantType := range want {
		gotType, ok := got[name]
		if !ok {
			t.Errorf("missing metric %q", name)
			continue
		}
		if gotType != wantType {
			t.Errorf("metric %q: want type %v, got %v", name, wantType, gotType)
		}
	}
}

// TestPreRegisteredLabelSets ensures every bounded label combination shows up
// in the output at zero count. Pre-initialisation is what lets operators
// alert on absence (e.g. rate(rcfgsim_sessions_total{result="error"}[5m]) == 0)
// without having to wait for traffic.
func TestPreRegisteredLabelSets(t *testing.T) {
	reg := New()
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	labelsByName := map[string]map[string]bool{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			key := labelKey(m)
			if labelsByName[mf.GetName()] == nil {
				labelsByName[mf.GetName()] = map[string]bool{}
			}
			labelsByName[mf.GetName()][key] = true
		}
	}

	cases := []struct {
		metric string
		want   []string
	}{
		{"rcfgsim_sessions_total", []string{
			"result=ok", "result=auth_fail", "result=disconnect", "result=error",
		}},
		{"rcfgsim_auth_attempts_total", []string{
			"result=ok", "result=fail",
		}},
		{"rcfgsim_faults_injected_total", []string{
			"type=auth_fail", "type=disconnect_mid", "type=slow_response", "type=malformed",
		}},
	}
	for _, tc := range cases {
		for _, k := range tc.want {
			if !labelsByName[tc.metric][k] {
				t.Errorf("%s: expected pre-registered series with labels %q, not found", tc.metric, k)
			}
		}
	}

	// Every known command should appear on the command_duration histogram.
	for _, cmd := range KnownCommands {
		if !labelsByName["rcfgsim_command_duration_seconds"]["command="+cmd] {
			t.Errorf("rcfgsim_command_duration_seconds: expected command=%s at registration", cmd)
		}
	}
}

// TestHandlerServesExpositionFormat exercises promhttp.Handler wired to the
// registry: the response must be 200 OK and contain every metric name.
func TestHandlerServesExpositionFormat(t *testing.T) {
	reg := New()
	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)
	for _, name := range []string{
		"rcfgsim_active_sessions",
		"rcfgsim_sessions_total",
		"rcfgsim_session_duration_seconds",
		"rcfgsim_command_duration_seconds",
		"rcfgsim_bytes_sent_total",
		"rcfgsim_auth_attempts_total",
		"rcfgsim_handshake_duration_seconds",
		"rcfgsim_faults_injected_total",
	} {
		if !strings.Contains(text, name) {
			t.Errorf("/metrics output missing metric %q", name)
		}
	}
	// Go runtime metric sanity: confirm promhttp actually added the standard
	// go_ collectors so regressions in registration surface here.
	if !strings.Contains(text, "go_goroutines") {
		t.Error("/metrics missing go_goroutines — Go collector not registered")
	}
}

func labelKey(m *dto.Metric) string {
	parts := make([]string, 0, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		parts = append(parts, lp.GetName()+"="+lp.GetValue())
	}
	return strings.Join(parts, ",")
}
