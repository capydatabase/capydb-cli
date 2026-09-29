package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestConnectOpensPsqlOnPooledURL runs `capydb connect` against a fake psql on
// PATH and checks that it is the psql command (not `link`), honours --pooled,
// makes the URL usable by libpq, and forwards arguments after --.
func TestConnectOpensPsqlOnPooledURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake psql is a shell script")
	}
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args")
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done > " + argsFile + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "psql"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake psql: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{"projects": []map[string]any{{"id": "prj_1", "name": "shop", "slug": "shop"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/prj_1/connections":
			writeJSON(t, w, map[string]any{"connections": map[string]any{
				"direct_url": "postgres://u:p@shop.db.capydb.dev:5432/db?sslmode=verify-full",
				"pooled_url": "postgres://u:p@shop.db.capydb.dev:6432/db?sslmode=verify-full",
			}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(), "connect", "--project", "shop", "--pooled",
		"--api-url", server.URL, "--api-key", "capy_test_key", "--", "-c", "select 1")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, output)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake psql was not run: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"postgres://u:p@shop.db.capydb.dev:6432/db?sslmode=verify-full&sslrootcert=system", "-c", "select 1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("psql args = %q, want %q", got, want)
	}
}

// `connect` used to alias `link`; its env-file flags must now fail loudly
// rather than be read as something else.
func TestConnectNoLongerAliasesLink(t *testing.T) {
	isolateUserConfig(t)
	_, err := runCommand(t, t.TempDir(), "connect", "--env-file", ".env")
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --env-file") {
		t.Fatalf("expected an unknown-flag error, got %v", err)
	}
}
