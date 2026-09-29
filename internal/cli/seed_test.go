package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDetectSeedPlanOrder(t *testing.T) {
	dir := t.TempDir()
	if _, err := detectSeedPlan(dir); err == nil {
		t.Fatal("expected an error with nothing to seed")
	}

	if err := os.MkdirAll(filepath.Join(dir, "supabase"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "supabase", "seed.sql"), "select 1;")
	plan, err := detectSeedPlan(dir)
	if err != nil || plan.Kind != "sql" || plan.File != filepath.Join(dir, "supabase", "seed.sql") {
		t.Fatalf("plan = %+v, %v; want supabase/seed.sql", plan, err)
	}

	writeTestFile(t, filepath.Join(dir, "package.json"), `{"prisma": {"seed": "tsx prisma/seed.ts"}}`)
	plan, err = detectSeedPlan(dir)
	if err != nil || plan.Command != "tsx prisma/seed.ts" {
		t.Fatalf("plan = %+v, %v; want the prisma seed command", plan, err)
	}

	writeTestFile(t, filepath.Join(dir, "package.json"), `{"scripts": {"seed": "x", "db:seed": "tsx seed.ts"}, "prisma": {"seed": "y"}}`)
	writeTestFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "")
	plan, err = detectSeedPlan(dir)
	if err != nil || plan.Command != "pnpm run db:seed" {
		t.Fatalf("plan = %+v, %v; want pnpm run db:seed", plan, err)
	}
}

func TestSeedEnvPointsEveryNameAtTheTarget(t *testing.T) {
	env := seedEnv([]string{"PATH=/bin", "DATABASE_URL=postgres://old", "MY_DB=postgres://old"}, "postgres://new", "MY_DB")
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if _, dup := values[key]; dup {
			t.Fatalf("duplicate %s", key)
		}
		values[key] = value
	}
	for _, name := range []string{"DATABASE_URL", "DATABASE_DIRECT_URL", "DIRECT_URL", "DATABASE_POOL_URL", "CAPYDB_DATABASE_URL", "MY_DB"} {
		if values[name] != "postgres://new" {
			t.Errorf("%s = %q", name, values[name])
		}
	}
	if values["PATH"] != "/bin" {
		t.Errorf("unrelated vars must be kept")
	}
}

func seedRoutes(t *testing.T, restorePoints *int) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/connections": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"connections": map[string]any{"direct_url": "postgres://u:p@db.example:5432/demo"}})
		},
		"POST /v1/projects/prj_1/restore-points": func(w http.ResponseWriter, r *http.Request) {
			*restorePoints++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["kind"] != "pitr" || !strings.HasPrefix(body["label"].(string), "before capydb seed") {
				t.Errorf("unexpected restore point request: %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{"restore_point": map[string]any{"id": "rp_1", "label": body["label"]}})
		},
	}
}

func TestSeedRefusesProductionWithoutConfirmation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	restorePoints := 0
	server := newFakeControlPlane(t, map[string]any{"environment": "production"}, seedRoutes(t, &restorePoints))
	dir := t.TempDir()

	output, err := runCommand(t, dir, "seed", "--run", "touch ran", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "refusing to seed production") {
		t.Fatalf("err = %v, want a refusal\n%s", err, output)
	}
	if fileExists(filepath.Join(dir, "ran")) || restorePoints != 0 {
		t.Fatal("the seed ran or a restore point was taken without confirmation")
	}
}

func TestSeedProductionTakesARestorePointAndInjectsTheDirectURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	restorePoints := 0
	server := newFakeControlPlane(t, map[string]any{"environment": "production"}, seedRoutes(t, &restorePoints))
	dir := t.TempDir()

	output, err := runCommand(t, dir, "seed", "--run", `printf %s "$DIRECT_URL" > url.txt`, "--confirm-production", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("seed: %v\n%s", err, output)
	}
	got, err := os.ReadFile(filepath.Join(dir, "url.txt"))
	if err != nil || string(got) != "postgres://u:p@db.example:5432/demo" {
		t.Fatalf("seed command saw DIRECT_URL=%q (%v)", got, err)
	}
	if restorePoints != 1 || !strings.Contains(output, "capydb restore --restore-point rp_1") {
		t.Fatalf("restore points = %d\n%s", restorePoints, output)
	}
}

func TestSeedNonProductionAndDryRun(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	restorePoints := 0
	server := newFakeControlPlane(t, nil, seedRoutes(t, &restorePoints))
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "seed.sql"), "select 1;")

	output, err := runCommand(t, dir, "seed", "--dry-run", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("seed --dry-run: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Would seed project demo (non_production)") || !strings.Contains(output, "seed.sql") {
		t.Fatalf("unexpected dry run output:\n%s", output)
	}
	if restorePoints != 0 {
		t.Fatal("dry run must not create a restore point")
	}

	if _, err := runCommand(t, dir, "seed", "notes.txt", "--api-key", "capy_test", "--api-url", server.URL); err == nil {
		t.Fatal("a non-.sql argument must be rejected")
	}
}

// TestSeedSQLFileAgainstPostgres runs a real seed file through psql,
// including a COPY FROM stdin block, and checks a failing file leaves nothing.
func TestSeedSQLFileAgainstPostgres(t *testing.T) {
	url := liveDatabaseURL(t)
	if _, err := psqlPath(); err != nil {
		t.Skip(err)
	}
	liveExec(t, url, "CREATE TABLE pets (id int PRIMARY KEY, name text NOT NULL)")
	dir := t.TempDir()
	good := filepath.Join(dir, "good.sql")
	writeTestFile(t, good, "INSERT INTO pets VALUES (1, 'capy');\nCOPY pets (id, name) FROM stdin;\n2\tbara\n\\.\n")
	bad := filepath.Join(dir, "bad.sql")
	writeTestFile(t, bad, "INSERT INTO pets VALUES (3, 'x');\nINSERT INTO pets VALUES (1, 'duplicate');\n")

	var out strings.Builder
	if err := runPsqlFile(t.Context(), &out, &out, url, good); err != nil {
		t.Fatalf("good seed: %v\n%s", err, out.String())
	}
	if err := runPsqlFile(t.Context(), &out, &out, url, bad); err == nil {
		t.Fatal("a failing seed must return an error")
	}
	rows := liveQueryRows(t, url, "SELECT count(*) AS n FROM pets")
	if count, _ := rows[0]["n"].(int64); count != 2 {
		t.Fatalf("pets = %v, want 2 (the failed file must roll back entirely)", rows[0]["n"])
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
