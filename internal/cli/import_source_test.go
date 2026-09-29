package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

// No test in this package may depend on the machine's DNS or its pg_dump:
// DNS answers fail by default and pg_dump is "not installed". Tests that need
// either swap the seam and restore it.
func init() {
	sourceLookup = func(string) ([]net.IP, error) { return nil, errors.New("no DNS in tests") }
	localPgDumpMajor = func(context.Context) (int, bool) { return 0, false }
}

func stubSourceSeams(t *testing.T, serverMajor, pgDumpMajor int) {
	t.Helper()
	previousServer, previousDump := sourceServerMajor, localPgDumpMajor
	sourceServerMajor = func(context.Context, string) (int, error) { return serverMajor, nil }
	localPgDumpMajor = func(context.Context) (int, bool) { return pgDumpMajor, pgDumpMajor > 0 }
	t.Cleanup(func() { sourceServerMajor, localPgDumpMajor = previousServer, previousDump })
}

// failOnAPI is a control plane that must not be called at all.
func failOnAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the API must not be called for a private source: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
}

func TestImportFromLoopbackRedirectsToFileBeforeAPI(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	stubSourceSeams(t, 17, 16)
	server := failOnAPI(t)
	defer server.Close()

	_, err := runCommand(t, t.TempDir(), "import", "--project", "shop", "--confirm",
		"--source-url", "postgres://app:s3cret@localhost:5432/app",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err == nil {
		t.Fatal("expected a redirect error for a loopback source")
	}
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != exitcode.UsageError {
		t.Fatalf("expected a usage error, got %v", err)
	}
	message := err.Error()
	for _, want := range []string{
		`CapyDB cannot reach "localhost": it is this machine (loopback)`,
		"pg_dump -Fc --no-owner --no-privileges -d 'postgres://app:xxxxx@localhost:5432/app' -f source.dump",
		"capydb import --file source.dump --project shop",
		"your pg_dump is 16 but the source runs Postgres 17",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message missing %q:\n%s", want, message)
		}
	}
	if strings.Contains(message, "s3cret") {
		t.Errorf("message leaked the source password:\n%s", message)
	}
}

func TestImportFollowFromPrivateNetworkExplainsFollowNeedsReach(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	stubSourceSeams(t, 17, 17)
	previous := sourceLookup
	sourceLookup = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.10.4")}, nil }
	t.Cleanup(func() { sourceLookup = previous })
	server := failOnAPI(t)
	defer server.Close()

	_, err := runCommand(t, t.TempDir(), "import", "--follow", "--confirm",
		"--source-url", "postgres://app@db.office.example/app",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err == nil {
		t.Fatal("expected a redirect error for a private source")
	}
	for _, want := range []string{"resolves to 192.168.10.4", "A --follow import streams changes", "capydb import --file source.dump"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "pg_dump is ") {
		t.Errorf("matching majors still produced pg_dump advice:\n%s", err)
	}
}

func TestImportPreflightFromLoopbackPointsAtLocalScan(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	stubSourceSeams(t, 16, 16)
	server := failOnAPI(t)
	defer server.Close()

	_, err := runCommand(t, t.TempDir(), "import", "preflight",
		"--source-url", "postgres://app@127.0.0.1:55432/app",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err == nil || !strings.Contains(err.Error(), "capydb migrate scan --source-url") {
		t.Fatalf("expected the preflight redirect, got %v", err)
	}
}

func TestImportPreflightNotesPgDumpMismatchAndSupabasePooler(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	stubSourceSeams(t, 0, 16)
	t.Setenv("SUPABASE_ACCESS_TOKEN", "sbp_test")

	supabase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/abcdefghijklmnop/config/database/pooler" {
			t.Errorf("unexpected Supabase request %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"database_type":"PRIMARY","db_host":"aws-1-eu-west-1.pooler.supabase.com","db_port":6543,"db_user":"postgres.abcdefghijklmnop","db_name":"postgres","pool_mode":"transaction"}]`))
	}))
	defer supabase.Close()
	previousBase := supabaseAPIBaseURL
	supabaseAPIBaseURL = supabase.URL
	t.Cleanup(func() { supabaseAPIBaseURL = previousBase })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{"projects": []map[string]any{{"id": "prj_1", "name": "shop", "slug": "shop"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/prj_1/imports/preflight":
			writeJSON(t, w, map[string]any{"preflight": map[string]any{
				"ok":     true,
				"checks": []map[string]any{{"name": "connectivity", "status": "pass"}},
				"source": map[string]any{"server_version": "PostgreSQL 17.6 on aarch64-unknown-linux-gnu"},
			}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(), "import", "preflight",
		"--source-url", "postgres://postgres.abcdefghijklmnop:pw@aws-0-eu-west-1.pooler.supabase.com:5432/postgres",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err != nil {
		t.Fatalf("import preflight: %v\n%s", err, output)
	}
	for _, want := range []string{
		"Supabase reports this project's pooler at aws-1-eu-west-1.pooler.supabase.com, not aws-0-eu-west-1.pooler.supabase.com",
		"note: if you dump locally for `capydb import --file`, your pg_dump is 16 but the source runs Postgres 17",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}
