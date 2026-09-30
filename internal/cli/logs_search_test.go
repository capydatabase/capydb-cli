package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestParseLogTime(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                     {},
		"2026-09-29T00:00:00Z": time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		"90m":                  now.Add(-90 * time.Minute),
		"12h":                  now.Add(-12 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
	}
	for value, want := range cases {
		got, err := parseLogTime("--since", value, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseLogTime(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "1.5d", "-2h", "0h"} {
		if _, err := parseLogTime("--since", bad, now); err == nil {
			t.Errorf("parseLogTime(%q): expected an error", bad)
		}
	}
}

func TestWriteLogEntriesShowsSQLState(t *testing.T) {
	var out bytes.Buffer
	writeLogEntries(&out, []api.ProjectLogEntry{
		{Timestamp: time.Now(), Severity: "error", Message: `relation "x" does not exist`, SQLState: "42P01"},
		{Timestamp: time.Now(), Severity: "log", Message: "checkpoint complete"},
	}, false)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.HasSuffix(lines[0], `ERROR   [42P01] relation "x" does not exist`) || !strings.HasSuffix(lines[1], "LOG     checkpoint complete") {
		t.Fatalf("lines:\n%s", out.String())
	}
}

func TestLogsSearchSendsFiltersAndPages(t *testing.T) {
	var query url.Values
	status := http.StatusOK
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/logs/search": func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.Query()
			if status != http.StatusOK {
				w.WriteHeader(status)
				writeJSON(t, w, map[string]any{"error": "log search is not enabled"})
				return
			}
			writeJSON(t, w, map[string]any{"search": map[string]any{
				"entries": []map[string]any{{
					"timestamp": "2026-09-30T10:00:00Z", "severity": "error", "message": "duplicate key", "cursor": "c1",
					"sqlstate": "23505", "pid": 42, "user": "owner", "database": "app",
				}},
				"next_cursor": "page2", "truncated": true,
			}})
		},
	})
	base := []string{"logs", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test"}

	output, err := runCommand(t, t.TempDir(), append(base, "--sqlstate", "23,40p01", "--search", "duplicate", "--severity", "error", "--since", "2026-09-29T00:00:00Z", "--limit", "50")...)
	if err != nil {
		t.Fatalf("search: %v\n%s", err, output)
	}
	for key, want := range map[string]string{"sqlstate": "23,40P01", "q": "duplicate", "severity": "error", "since": "2026-09-29T00:00:00Z", "limit": "50"} {
		if query.Get(key) != want {
			t.Errorf("query %s = %q, want %q", key, query.Get(key), want)
		}
	}
	if query.Get("until") != "" || query.Get("cursor") != "" {
		t.Errorf("unexpected query: %v", query)
	}
	if !strings.Contains(output, "[23505] duplicate key") || !strings.Contains(output, "--cursor page2") {
		t.Fatalf("output:\n%s", output)
	}

	output, err = runCommand(t, t.TempDir(), append(base, "--cursor", "page2", "-o", "json")...)
	if err != nil || query.Get("cursor") != "page2" {
		t.Fatalf("cursor page: %v (query %v)", err, query)
	}
	var payload struct {
		Search api.ProjectLogSearch `json:"search"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil || payload.Search.Entries[0].PID != 42 || !payload.Search.Truncated {
		t.Fatalf("json = %s (%v)", output, err)
	}

	status = http.StatusServiceUnavailable
	if _, err := runCommand(t, t.TempDir(), append(base, "--sqlstate", "42P01")...); err == nil || !strings.Contains(err.Error(), "log search is not enabled on this deployment") {
		t.Fatalf("503 error = %v", err)
	}
}

func TestLogsSearchRejectsFollowAndBadCodes(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "--follow", "--sqlstate", "42P01"},
		{"logs", "--sqlstate", "4201"},
		{"logs", "--since", "yesterday"},
	} {
		if _, err := runCommand(t, t.TempDir(), append(args, "--api-url", "http://127.0.0.1:1", "--api-key", "capy_test")...); err == nil {
			t.Errorf("%v: expected a usage error", args)
		}
	}
}
