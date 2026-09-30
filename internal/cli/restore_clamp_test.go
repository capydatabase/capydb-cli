package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestRestoreClampNotice(t *testing.T) {
	requested := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	effective := requested.Add(-90 * time.Second)

	if got := restoreClampNotice(nil); got != "" {
		t.Fatalf("nil pitr notice = %q", got)
	}
	if got := restoreClampNotice(&api.PITRRestoreTarget{RequestedRestoreTime: requested, RestoreTime: requested}); got != "" {
		t.Fatalf("unclamped notice = %q", got)
	}
	got := restoreClampNotice(&api.PITRRestoreTarget{RequestedRestoreTime: requested, RestoreTime: effective, RestoreTimeClamped: true})
	for _, want := range []string{"2026-09-30T10:00:00Z is past the latest restorable point", "runs to 2026-09-30T09:58:30Z", "1m30s earlier"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice %q missing %q", got, want)
		}
	}
}

func TestRestorePrintsTheClampedTime(t *testing.T) {
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/restores": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{
				"job": map[string]any{"id": "job_r", "state": "queued", "type": "project.restore"},
				"pitr": map[string]any{
					"requested_restore_time": "2026-09-30T10:00:00Z",
					"restore_time":           "2026-09-30T09:59:00Z",
					"restore_time_clamped":   true,
				},
			})
		},
	})
	output, err := runCommand(t, t.TempDir(), "restore", "--project", "prj_1", "--restore-time", "2026-09-30T10:00:00Z", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, output)
	}
	if !strings.Contains(output, "runs to 2026-09-30T09:59:00Z instead (1m0s earlier)") {
		t.Fatalf("output missing the clamp notice:\n%s", output)
	}
}
