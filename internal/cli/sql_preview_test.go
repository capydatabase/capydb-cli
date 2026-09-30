package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSQLPreviewRunsAgainstThePreview(t *testing.T) {
	var body map[string]any
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"POST /v1/preview-databases/prv_1/sql": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, w, map[string]any{"result": map[string]any{
				"columns": []string{"n"}, "rows": []map[string]any{{"n": 1}}, "row_count": 1, "duration_ms": 3, "truncated": false,
			}})
		},
	})
	output, err := runCommand(t, t.TempDir(), "sql", "delete from events", "--preview", "prv_1", "--allow-unqualified-writes", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("sql --preview: %v\n%s", err, output)
	}
	if body["query"] != "delete from events" || body["allow_unqualified_writes"] != true {
		t.Fatalf("request body = %v", body)
	}
	if !strings.Contains(output, "n") || !strings.Contains(output, "1") {
		t.Fatalf("output:\n%s", output)
	}
}

func TestSQLPreviewAndProjectAreExclusive(t *testing.T) {
	_, err := runCommand(t, t.TempDir(), "sql", "select 1", "--preview", "prv_1", "--project", "prj_1", "--api-url", "http://127.0.0.1:1", "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v", err)
	}
}
