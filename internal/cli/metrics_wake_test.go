package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestWriteWakeLatency(t *testing.T) {
	p50, p95, maxMs := 148.0, 1240.5, int64(1800)
	cases := []struct {
		wake *api.ProjectWakeLatency
		want string
	}{
		{nil, ""},
		{&api.ProjectWakeLatency{WindowHours: 168}, "resumes (last 7 days): none\n"},
		{&api.ProjectWakeLatency{Wakes: 2, WindowHours: 168}, "resumes (last 7 days): 2, none timed\n"},
		{&api.ProjectWakeLatency{Wakes: 5, TimedWakes: 4, P50Ms: &p50, P95Ms: &p95, MaxMs: &maxMs, WindowHours: 168},
			"resumes (last 7 days): 5, p50 148ms, p95 1.2s, max 1.8s (4 timed)\n"},
		{&api.ProjectWakeLatency{Wakes: 1, TimedWakes: 1, P50Ms: &p50, WindowHours: 24}, "resumes (last 24h): 1, p50 148ms\n"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		writeWakeLatency(&out, tc.wake)
		if out.String() != tc.want {
			t.Errorf("writeWakeLatency(%+v) = %q, want %q", tc.wake, out.String(), tc.want)
		}
	}

	var report bytes.Buffer
	writeObservabilityReport(&report, api.ProjectObservability{Wake: &api.ProjectWakeLatency{WindowHours: 168}})
	if !strings.Contains(report.String(), "resumes (last 7 days): none") {
		t.Fatalf("metrics report missing the resume block:\n%s", report.String())
	}
}
