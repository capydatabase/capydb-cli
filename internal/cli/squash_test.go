package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

// fakeCapysquash writes an executable that records its argv and exits with
// the given code, and points the lookup override at it.
func fakeCapysquash(t *testing.T, exitCode int) (argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary")
	}
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	script := filepath.Join(dir, "capysquash")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\nexit " + map[int]string{0: "0", 2: "2"}[exitCode] + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := capysquashLookPath
	capysquashLookPath = func(name string) (string, error) {
		if name == "capysquash" {
			return script, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { capysquashLookPath = previous })
	return argvFile
}

func fakeManagedCapysquash(t *testing.T) (invocations string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary")
	}
	dir := t.TempDir()
	invocations = filepath.Join(dir, "invocations")
	script := filepath.Join(dir, "capysquash")
	body := `#!/bin/sh
printf '%s\n' "$*" >> '` + invocations + `'
if [ "$1" = "squash" ]; then
  shift
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "--output" ]; then
      shift
      output="$1"
      break
    fi
    shift
  done
  mkdir -p "$output"
  printf 'CREATE TABLE users (id bigint);\n' > "$output/0001_squashed.sql"
  exit 0
fi
snapshot=""
against=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --snapshot-output) shift; snapshot="$1" ;;
    --against-snapshot) shift; against="$1" ;;
  esac
  shift
done
if [ -n "$snapshot" ]; then
  printf '{"contract_version":"capysquash.catalog-snapshot.v1","postgresql_version":"17.6","signature":[]}\n' > "$snapshot"
  printf '{"contract_version":"pgsquash.external-validation.v1","success":true,"phase":"snapshot","comparison_valid":false,"has_differences":false,"differences":[]}\n'
  exit 0
fi
if [ -n "$against" ]; then
  printf '{"contract_version":"capysquash.external-validation.v1","success":true,"phase":"compare","comparison_valid":true,"has_differences":false,"differences":[]}\n'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := capysquashLookPath
	capysquashLookPath = func(name string) (string, error) {
		if name == "capysquash" {
			return script, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { capysquashLookPath = previous })
	return invocations
}

func TestMigrateSquashMissingBinaryExplainsInstall(t *testing.T) {
	previous := capysquashLookPath
	capysquashLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { capysquashLookPath = previous })

	_, err := runCommand(t, t.TempDir(), "migrate", "squash")
	if err == nil || !strings.Contains(err.Error(), "go install github.com/capydatabase/capysquash/cmd/capysquash") {
		t.Fatalf("expected the install hint, got %v", err)
	}
}

func TestMigrateSquashAnalyzeResolvesSupabaseDirAndPrintsFooter(t *testing.T) {
	argvFile := fakeCapysquash(t, 0)
	root := t.TempDir()
	migrations := filepath.Join(root, "supabase", "migrations")
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"0002_second.sql", "0001_first.sql"} {
		if err := os.WriteFile(filepath.Join(migrations, name), []byte("select 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	output, err := runCommand(t, root, "migrate", "squash", root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	argv, readErr := os.ReadFile(argvFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	want := []string{"analyze", filepath.Join(migrations, "0001_first.sql"), filepath.Join(migrations, "0002_second.sql")}
	if len(lines) != 3 || lines[0] != want[0] || lines[1] != want[1] || lines[2] != want[2] {
		t.Fatalf("child argv = %q, want %q (sorted files, not the directory)", lines, want)
	}
	if !strings.Contains(output, "nothing was changed") || !strings.Contains(output, "--workflow safe") {
		t.Fatalf("expected the analyze footer, got:\n%s", output)
	}
}

func TestMigrateSquashWorkflowSafePassesThroughAndSkipsFooter(t *testing.T) {
	argvFile := fakeCapysquash(t, 0)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "0001_init.sql"), []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}

	output, err := runCommand(t, root, "migrate", "squash", "--workflow", "safe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	if !strings.HasPrefix(string(argv), "safe\n") {
		t.Fatalf("child argv = %q, want safe first", string(argv))
	}
	if strings.Contains(output, "nothing was changed") {
		t.Fatalf("safe workflow must not print the analyze footer:\n%s", output)
	}
}

func TestMigrateSquashFindsNestedPrismaMigrations(t *testing.T) {
	argvFile := fakeCapysquash(t, 0)
	root := t.TempDir()
	migration := filepath.Join(root, "prisma", "migrations", "20260902090000_init", "migration.sql")
	if err := os.MkdirAll(filepath.Dir(migration), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(migration, []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runCommand(t, root, "migrate", "squash", root); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), migration) {
		t.Fatalf("nested Prisma migration was not passed to capysquash: %s", argv)
	}
}

func TestMigrateSquashChildFailureMapsToGenericExit(t *testing.T) {
	fakeCapysquash(t, 2)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "0001_init.sql"), []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runCommand(t, root, "migrate", "squash")
	coded := &exitcode.Error{}
	if !errors.As(err, &coded) || coded.Code != exitcode.GenericError {
		t.Fatalf("expected a GenericError exit mapping, got %v", err)
	}
}

func TestMigrateSquashRejectsUnknownWorkflow(t *testing.T) {
	_, err := runCommand(t, t.TempDir(), "migrate", "squash", "--workflow", "yolo")
	if err == nil || !strings.Contains(err.Error(), "unknown --workflow") {
		t.Fatalf("expected a usage error, got %v", err)
	}
}

func TestMigrateSquashCapyDBValidationEndToEnd(t *testing.T) {
	t.Setenv("CI", "true")
	invocations := fakeManagedCapysquash(t)
	var connectionFetches atomic.Int32
	var previewDeletes atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{"projects": []map[string]any{{
				"id": "project_1", "name": "test-app", "slug": "test-app", "state": "ready",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/project_1/preview-databases":
			writeJSON(t, w, map[string]any{
				"preview": map[string]any{"id": "preview_1", "project_id": "project_1", "name": "squash-check", "mode": "empty", "state": "provisioning", "ttl_expires_at": "2026-09-02T12:00:00Z"},
				"job":     map[string]any{"id": "job_create", "state": "pending", "type": "branch.create"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_create":
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_create", "state": "completed", "type": "branch.create"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/preview-databases/preview_1/connections":
			connectionFetches.Add(1)
			writeJSON(t, w, map[string]any{"connections": map[string]any{
				"direct_url": "postgresql://preview:secret@db.example.test:5432/preview?sslmode=require",
				"username":   "preview",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/preview-databases/preview_1/reset":
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_reset", "state": "pending", "type": "branch.reset"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_reset":
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_reset", "state": "completed", "type": "branch.reset"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/preview-databases/preview_1":
			previewDeletes.Add(1)
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_delete", "state": "pending", "type": "branch.delete"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_delete":
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_delete", "state": "completed", "type": "branch.delete"}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	migrations := filepath.Join(root, "migrations")
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "0001_init.sql"), []byte("CREATE TABLE users (id bigint);"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(root, "baseline")

	output, err := runCommand(t, root,
		"migrate", "squash", migrations,
		"--workflow", "safe",
		"--validation", "capydb",
		"--project", "test-app",
		"--output", outputDir,
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	)
	if err != nil {
		t.Fatalf("managed squash failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "0001_squashed.sql")); err != nil {
		t.Fatalf("validated output was not published: %v", err)
	}
	if connectionFetches.Load() != 2 {
		t.Fatalf("connection fetches = %d, want 2", connectionFetches.Load())
	}
	if previewDeletes.Load() != 1 {
		t.Fatalf("preview deletes = %d, want 1", previewDeletes.Load())
	}
	if !strings.Contains(output, "Validated squash written") {
		t.Fatalf("missing success output:\n%s", output)
	}

	argv, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "secret") || strings.Contains(string(argv), "postgresql://") {
		t.Fatalf("database credentials were passed on the command line:\n%s", argv)
	}
	if !strings.Contains(string(argv), "--safety conservative") || strings.Count(string(argv), "validate-external") != 2 {
		t.Fatalf("unexpected capysquash invocations:\n%s", argv)
	}
}
