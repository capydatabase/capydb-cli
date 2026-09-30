package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// lintPayload is a GET .../lint response in the spec's shape.
func lintPayload(findings []map[string]any, skipped []string) map[string]any {
	return map[string]any{"lint": map[string]any{"findings": findings, "skipped": skipped}}
}

func TestDBLintRendersTheServerReport(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/lint": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, lintPayload([]map[string]any{
				{"rule": "missing_primary_key", "severity": "warning", "object": "public.logs", "message": "no primary key"},
				{"rule": "unindexed_foreign_key", "severity": "warning", "object": "public.orders", "message": "orders_user_id_fkey (user_id) has no index", "fix": `CREATE INDEX CONCURRENTLY ON "public"."orders" (user_id);`},
				{"rule": "unused_index", "severity": "info", "object": "public.orders_note_idx", "message": "no scans in 10 days"},
			}, []string{"table_bloat: statistics unavailable"}))
		},
	})

	output, err := runCommand(t, t.TempDir(), "db", "lint", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test", "--exit-code")
	if err == nil || !strings.Contains(err.Error(), "2 lint warning(s)") {
		t.Fatalf("expected --exit-code to fail on two warnings, got %v\n%s", err, output)
	}
	for _, want := range []string{
		"[warning] missing_primary_key public.logs: no primary key",
		`    fix: CREATE INDEX CONCURRENTLY ON "public"."orders" (user_id);`,
		"[info] unused_index public.orders_note_idx",
		"[skip] table_bloat: statistics unavailable",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in\n%s", want, output)
		}
	}
}

func TestDBLintPreviewJSONKeepsTheCLIShape(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/preview-databases/pdb_1/lint": func(w http.ResponseWriter, r *http.Request) {
			// A server that serializes empty lists as null still yields [].
			writeJSON(t, w, map[string]any{"lint": map[string]any{"findings": nil, "skipped": []string{"unused and redundant index checks do not run against a preview"}}})
		},
	})
	output, err := runCommand(t, t.TempDir(), "db", "lint", "--preview", "pdb_1", "--api-url", server.URL, "--api-key", "capy_test", "--output", "json")
	if err != nil {
		t.Fatalf("db lint --preview: %v\n%s", err, output)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if string(report["findings"]) != "[]" || !strings.Contains(string(report["skipped"]), "preview") || len(report) != 2 {
		t.Fatalf("unexpected JSON shape:\n%s", output)
	}
}

func TestDBLintCleanReport(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/lint": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, lintPayload([]map[string]any{}, []string{}))
		},
	})
	output, err := runCommand(t, t.TempDir(), "db", "lint", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test", "--exit-code")
	if err != nil || !strings.Contains(output, "No schema or index problems found.") {
		t.Fatalf("clean lint: %v\n%s", err, output)
	}
}
