package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rlsFixtureSQL = `
create table public.todos (
  id bigint generated always as identity primary key,
  owner_id uuid not null default auth.uid(),
  title text
);
alter table public.todos enable row level security;

create policy todos_owner on public.todos
  for all to authenticated
  using (auth.uid() = owner_id)
  with check (auth.uid() = owner_id);

create policy org_read on public.todos
  for select to authenticated
  using (owner_id::text = auth.jwt() ->> 'org_id');
`

func writeRLSFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	migrations := filepath.Join(dir, "supabase", "migrations")
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "0001_init.sql"), []byte(rlsFixtureSQL), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMigrateRLSFindsSupabaseMigrationsAndConverts(t *testing.T) {
	dir := writeRLSFixture(t)
	output, err := runCommand(t, dir, "migrate", "rls", dir, "--out", filepath.Join(dir, "capyrls"))
	if err != nil {
		t.Fatalf("migrate rls: %v\n%s", err, output)
	}
	for _, want := range []string{
		"supabase/migrations",
		"policies: 2 converted, 0 skipped, 0 need attention",
		"app.user_id (uuid",
		"app.org_id (text)",
	} {
		if !strings.Contains(output, filepath.FromSlash(want)) && !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}

	prelude, err := os.ReadFile(filepath.Join(dir, "capyrls", "capyrls_01_prelude.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prelude), "app.user_id()") {
		t.Error("prelude missing app.user_id accessor")
	}

	// CapyDB default is the single-role model: the app connects as the
	// owning credential, so the bundle must FORCE row security.
	force, err := os.ReadFile(filepath.Join(dir, "capyrls", "capyrls_02_force_rls.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(force), "force row level security") {
		t.Error("single-role bundle missing FORCE ROW LEVEL SECURITY")
	}

	policies, err := os.ReadFile(filepath.Join(dir, "capyrls", "capyrls_03_policies.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(policies), "auth.uid()") {
		t.Error("policies still reference auth.uid()")
	}
	if _, err := os.Stat(filepath.Join(dir, "capyrls", "capyrls_report.md")); err != nil {
		t.Error("report not written")
	}
}

// CapyDB is the default target: the split model there grants to the
// platform's runtime role, checking it exists, and creates no roles.
func TestMigrateRLSSplitRoleModelOnCapyDB(t *testing.T) {
	dir := writeRLSFixture(t)
	for _, mode := range []string{"vanilla", "supabase-compat"} {
		out := filepath.Join(dir, "capyrls-"+mode)
		output, err := runCommand(t, dir, "migrate", "rls", dir, "--role-model", "split", "--mode", mode, "--out", out)
		if err != nil {
			t.Fatalf("mode %s: migrate rls: %v\n%s", mode, err, output)
		}
		roles, err := os.ReadFile(filepath.Join(out, "capyrls_02_roles.sql"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"rolname = 'app_user'", "roles/app", "grant usage on schema public to app_user;"} {
			if !strings.Contains(string(roles), want) {
				t.Errorf("mode %s: roles file missing %q:\n%s", mode, want, roles)
			}
		}
		for _, unwanted := range []string{"create role ", "app_service"} {
			if strings.Contains(string(roles), unwanted) {
				t.Errorf("mode %s: roles file contains %q; a CapyDB database role cannot create roles:\n%s", mode, unwanted, roles)
			}
		}
	}
}

func TestMigrateRLSCompatSingleHasServiceEscape(t *testing.T) {
	dir := writeRLSFixture(t)
	output, err := runCommand(t, dir, "migrate", "rls", dir, "--mode", "supabase-compat", "--out", filepath.Join(dir, "capyrls"))
	if err != nil {
		t.Fatalf("migrate rls: %v\n%s", err, output)
	}
	force, err := os.ReadFile(filepath.Join(dir, "capyrls", "capyrls_02_force_rls.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(force), "using (auth.role() = 'service_role')") {
		t.Errorf("compat bundle lacks the claims-based service escape:\n%s", force)
	}
}

func TestMigrateRLSInvalidTargetIsUsageError(t *testing.T) {
	dir := writeRLSFixture(t)
	_, err := runCommand(t, dir, "migrate", "rls", dir, "--target", "rds")
	if err == nil || !strings.Contains(err.Error(), "--target") {
		t.Fatalf("expected a usage error for a bad --target, got %v", err)
	}
}

func TestMigrateRLSSplitRoleModel(t *testing.T) {
	dir := writeRLSFixture(t)
	output, err := runCommand(t, dir, "migrate", "rls", dir, "--role-model", "split", "--target", "postgres", "--out", filepath.Join(dir, "capyrls"))
	if err != nil {
		t.Fatalf("migrate rls: %v\n%s", err, output)
	}
	roles, err := os.ReadFile(filepath.Join(dir, "capyrls", "capyrls_02_roles.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(roles), "create role app_user") {
		t.Error("split model must create the runtime role")
	}
}

func TestMigrateRLSJSONOutput(t *testing.T) {
	dir := writeRLSFixture(t)
	output, err := runCommand(t, dir, "-o", "json", "migrate", "rls", dir, "--out", filepath.Join(dir, "capyrls"))
	if err != nil {
		t.Fatalf("migrate rls: %v\n%s", err, output)
	}
	var payload struct {
		RLS struct {
			Files  []string `json:"files"`
			Report struct {
				Mode      string `json:"mode"`
				RoleModel string `json:"role_model"`
				Policies  []struct {
					Status string `json:"status"`
				} `json:"policies"`
			} `json:"report"`
		} `json:"rls"`
	}
	// The apply-order hint goes to stderr, which runCommand merges into the
	// same buffer; decode only the JSON document.
	jsonStart := strings.Index(output, "{")
	decoder := json.NewDecoder(strings.NewReader(output[jsonStart:]))
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("JSON output not parseable: %v\n%s", err, output)
	}
	if payload.RLS.Report.Mode != "vanilla" || payload.RLS.Report.RoleModel != "single" {
		t.Errorf("unexpected defaults in JSON report: %+v", payload.RLS.Report)
	}
	if len(payload.RLS.Files) == 0 || len(payload.RLS.Report.Policies) != 2 {
		t.Errorf("JSON payload incomplete: %+v", payload.RLS)
	}
}

// Clerk-style subjects (user_2abc...) are not uuids: --uid-type text makes the
// accessor return text, and the fixture's uuid owner_id column is reported as
// the thing to change before applying.
func TestMigrateRLSUIDTypeText(t *testing.T) {
	dir := writeRLSFixture(t)
	output, err := runCommand(t, dir, "-o", "json", "migrate", "rls", dir, "--uid-type", "text", "--out", filepath.Join(dir, "capyrls"))
	if err != nil {
		t.Fatalf("migrate rls: %v\n%s", err, output)
	}
	var payload struct {
		RLS struct {
			Report struct {
				UIDType string `json:"uid_type"`
				GUCs    []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"gucs"`
				Warnings []string `json:"warnings"`
			} `json:"report"`
		} `json:"rls"`
	}
	jsonStart := strings.Index(output, "{")
	if err := json.NewDecoder(strings.NewReader(output[jsonStart:])).Decode(&payload); err != nil {
		t.Fatalf("JSON output not parseable: %v\n%s", err, output)
	}
	report := payload.RLS.Report
	if report.UIDType != "text" {
		t.Errorf("uid_type = %q, want text", report.UIDType)
	}
	if len(report.GUCs) == 0 || report.GUCs[0].Name != "app.user_id" || report.GUCs[0].Type != "text" {
		t.Errorf("app.user_id should be typed text: %+v", report.GUCs)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "uuid column public.todos.owner_id") {
		t.Errorf("warnings should name the uuid owner_id column: %v", report.Warnings)
	}

	prelude, err := os.ReadFile(filepath.Join(dir, "capyrls", "capyrls_01_prelude.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prelude), "returns text") || strings.Contains(string(prelude), "::uuid") {
		t.Errorf("prelude should define a text accessor without a uuid cast:\n%s", prelude)
	}
}

func TestMigrateRLSInvalidUIDTypeIsUsageError(t *testing.T) {
	dir := writeRLSFixture(t)
	_, err := runCommand(t, dir, "migrate", "rls", dir, "--uid-type", "int")
	if err == nil || !strings.Contains(err.Error(), "--uid-type") {
		t.Fatalf("expected a usage error for a bad --uid-type, got %v", err)
	}
}

func TestMigrateRLSInvalidFlagIsUsageError(t *testing.T) {
	dir := writeRLSFixture(t)
	_, err := runCommand(t, dir, "migrate", "rls", dir, "--mode", "nonsense")
	if err == nil || !strings.Contains(err.Error(), "--mode") {
		t.Fatalf("expected a usage error for a bad --mode, got %v", err)
	}
}

func TestMigrateRLSNoSQLFilesIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := runCommand(t, dir, "migrate", "rls", dir)
	if err == nil || !strings.Contains(err.Error(), "no .sql files") {
		t.Fatalf("expected a no-input error, got %v", err)
	}
}
