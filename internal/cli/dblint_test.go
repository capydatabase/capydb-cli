package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestLintSchemaFlagsMissingPrimaryKeys(t *testing.T) {
	schema := api.DatabaseSchema{Schemas: []api.SchemaNamespace{{
		Name: "public",
		Tables: []api.SchemaTable{
			{Name: "ok", Kind: "table", PrimaryKey: []string{"id"}},
			{Name: "events", Kind: "table"},
			{
				Name: "accounts", Kind: "table",
				Columns:           []api.SchemaColumn{{Name: "email"}, {Name: "nick", IsNullable: true}},
				UniqueConstraints: []api.SchemaUniqueConstraint{{Name: "accounts_nick_key", Columns: []string{"nick"}}, {Name: "accounts_email_key", Columns: []string{"email"}}},
			},
			{Name: "a_view", Kind: "view"},
			{Name: "remote", Kind: "foreign_table"},
		},
	}}}
	findings := lintSchema(schema)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want events and accounts", findings)
	}
	sortLintFindings(findings)
	if findings[0].Object != "public.accounts" || !strings.Contains(findings[0].Message, "accounts_email_key") {
		t.Fatalf("accounts finding should point at the NOT NULL unique constraint: %+v", findings[0])
	}
	if findings[1].Object != "public.events" || findings[1].Severity != lintWarning {
		t.Fatalf("unexpected finding: %+v", findings[1])
	}
}

// TestLintCatalogQueryAgainstPostgres pins the catalog query's semantics on a
// real server: which foreign keys count as covered, what a duplicate is, and
// that the scope excludes other schemas.
func TestLintCatalogQueryAgainstPostgres(t *testing.T) {
	url := liveDatabaseURL(t)
	liveExec(t, url, `
CREATE TABLE parents (id int PRIMARY KEY, code text, UNIQUE (id, code));
CREATE TABLE covered (id int PRIMARY KEY, parent_id int REFERENCES parents);
CREATE INDEX covered_parent_idx ON covered (parent_id, id);
CREATE TABLE uncovered (id int PRIMARY KEY, parent_id int REFERENCES parents);
CREATE INDEX uncovered_wrong_order ON uncovered (id, parent_id);
CREATE TABLE multi (id int PRIMARY KEY, pid int, pcode text, FOREIGN KEY (pid, pcode) REFERENCES parents (id, code));
CREATE INDEX multi_reversed ON multi (pcode, pid);
CREATE TABLE partial_only (id int PRIMARY KEY, parent_id int REFERENCES parents);
CREATE INDEX partial_only_idx ON partial_only (parent_id) WHERE parent_id > 0;
CREATE TABLE dupes (id int PRIMARY KEY, a int, b int);
CREATE INDEX dupes_a_1 ON dupes (a);
CREATE INDEX dupes_a_2 ON dupes (a);
CREATE INDEX dupes_a_desc ON dupes (a DESC);
CREATE INDEX dupes_id ON dupes (id);
CREATE SCHEMA other;
CREATE TABLE other.child (id int PRIMARY KEY, parent_id int REFERENCES public.parents);
`)
	rows := liveQueryRows(t, url, lintCatalogQuery([]string{"public"}))
	// Round-trip through JSON so the values have the SQL endpoint's shapes.
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	findings := lintCatalogFindings(decoded)

	got := map[string]lintFinding{}
	for _, finding := range findings {
		got[finding.Rule+" "+finding.Object] = finding
	}
	for _, want := range []string{
		"unindexed_foreign_key public.uncovered (uncovered_parent_id_fkey)",
		"unindexed_foreign_key public.partial_only (partial_only_parent_id_fkey)",
		"duplicate_index public.dupes",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing finding %q in %+v", want, findings)
		}
	}
	if len(findings) != 4 {
		t.Errorf("findings = %d, want 4 (two unindexed FKs, two duplicate groups): %+v", len(findings), findings)
	}
	for _, finding := range findings {
		switch {
		case strings.HasPrefix(finding.Object, "public.covered "), strings.Contains(finding.Object, "multi"), strings.Contains(finding.Object, "other."):
			t.Errorf("unexpected finding: %+v", finding)
		case finding.Rule == "duplicate_index" && strings.Contains(finding.Message, "dupes_a_desc"):
			t.Errorf("a DESC index is not a duplicate of an ASC one: %+v", finding)
		case finding.Rule == "duplicate_index" && strings.Contains(finding.Message, "dupes_id") &&
			!strings.HasPrefix(finding.Message, "identical indexes: dupes_pkey"):
			t.Errorf("the primary key must be the index to keep: %+v", finding)
		}
	}
	if fk := got["unindexed_foreign_key public.uncovered (uncovered_parent_id_fkey)"]; fk.Fix != `CREATE INDEX CONCURRENTLY ON "public"."uncovered" (parent_id);` {
		t.Errorf("fix = %q", fk.Fix)
	}
}

func TestDBLintCommandCombinesSources(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var sqlBody map[string]any
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/schema": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"schema": map[string]any{"schemas": []any{map[string]any{
				"name":   "public",
				"tables": []any{map[string]any{"name": "logs", "kind": "table"}},
			}}}})
		},
		"POST /v1/projects/prj_1/sql": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&sqlBody)
			writeJSON(t, w, map[string]any{"result": map[string]any{"rows": []any{map[string]any{
				"rule": "unindexed_foreign_key", "schema_name": "public", "table_name": "orders",
				"object_name": "orders_user_id_fkey", "detail": "user_id", "amount": 0,
			}}}})
		},
		"GET /v1/projects/prj_1/advisor/index-hygiene": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"hygiene": map[string]any{
				"available": true, "observation_window_seconds": 864000,
				"unused_indexes": []any{map[string]any{"schema": "public", "table": "orders", "index": "orders_note_idx", "size_bytes": 8192, "drop_statement": "DROP INDEX CONCURRENTLY public.orders_note_idx;"}},
			}})
		},
	})

	output, err := runCommand(t, t.TempDir(), "db", "lint", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test", "--exit-code")
	if err == nil {
		t.Fatalf("expected --exit-code to fail on warnings\n%s", output)
	}
	for _, want := range []string{"missing_primary_key public.logs", "unindexed_foreign_key public.orders", "unused_index public.orders_note_idx"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in\n%s", want, output)
		}
	}
	if sqlBody["read_only"] != true {
		t.Errorf("catalog query must run read-only: %+v", sqlBody)
	}
}

func TestDBLintPreviewSkipsLiveChecks(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/preview-databases/pdb_1/schema": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"schema": map[string]any{"schemas": []any{}}})
		},
	})
	output, err := runCommand(t, t.TempDir(), "db", "lint", "--preview", "pdb_1", "--api-url", server.URL, "--api-key", "capy_test", "--output", "json")
	if err != nil {
		t.Fatalf("db lint --preview: %v\n%s", err, output)
	}
	if !strings.Contains(output, `"findings": []`) || !strings.Contains(output, "do not run against a preview") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}
