package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// kvTestServer stands in for the control plane for the K/V commands: viewer,
// project lookup, and whichever K/V routes the test needs.
func kvTestServer(t *testing.T, projectID string, routes map[string]func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if handler, ok := routes[key]; ok {
			handler(w, r)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeJSON(t, w, map[string]any{
				"organization": map[string]any{
					"id": "org_kv", "name": "KV Org", "billing_plan": "ship", "billing_status": "active",
				},
				"principal": map[string]any{"organization_id": "org_kv", "scopes": []string{"*"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			// resolveProject resolves --project through the list endpoint.
			writeJSON(t, w, map[string]any{"projects": []map[string]any{{
				"id": projectID, "organization_id": "org_kv", "region": "swedencentral",
				"name": "kv-app", "slug": "kv-app", "state": "ready",
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID:
			writeJSON(t, w, map[string]any{"project": map[string]any{
				"id": projectID, "organization_id": "org_kv", "region": "swedencentral",
				"name": "kv-app", "slug": "kv-app", "state": "ready",
			}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func runKVCommand(t *testing.T, cwd string, args ...string) (string, error) {
	t.Helper()
	stdout, stderr, err := runKVCommandStreams(t, cwd, args...)
	return stdout + stderr, err
}

// runKVCommandStreams keeps the two streams apart, for the assertions that are
// about which stream a value lands on rather than whether it was printed.
func runKVCommandStreams(t *testing.T, cwd string, args ...string) (string, string, error) {
	t.Helper()
	application := &app{cwd: cwd}
	command := newRootCommand(application, "test")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(args)
	err := command.Execute()
	return stdout.String(), stderr.String(), err
}

// The token exists in exactly one response. If `create` failed to print it the
// customer would have provisioned an unusable store, so this is the assertion
// that matters most on this command.
func TestKVCreatePrintsTheTokenOnce(t *testing.T) {
	projectID := "project_kv"
	server := kvTestServer(t, projectID, map[string]func(http.ResponseWriter, *http.Request){
		"POST /v1/projects/" + projectID + "/kv": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{
				"job": map[string]any{"id": "job_kv_1", "state": "pending", "type": "kv.create"},
				"kv_store": map[string]any{
					"id": "kv_1", "project_id": projectID, "state": "provisioning",
					"maxmemory_mb": 128, "mem_max_mb": 256,
					"maxmemory_policy": "volatile-lru", "persistence": "rdb",
					"token": "capy_kv_secret",
					"credentials": map[string]any{
						"rest_url":   "https://kv-app-abc123.db.capydb.dev",
						"rest_token": "capy_kv_secret",
						"redis_url":  "rediss://default:capy_kv_secret@kv-app-abc123.db.capydb.dev:6379",
					},
				},
			})
		},
	})
	defer server.Close()

	out, err := runKVCommand(t, t.TempDir(),
		"kv", "create", "--api-url", server.URL, "--api-key", "capy_test_key", "--project", projectID)
	if err != nil {
		t.Fatalf("execute kv create: %v\n%s", err, out)
	}
	for _, expected := range []string{
		"CAPYKV_REST_URL=https://kv-app-abc123.db.capydb.dev",
		"CAPYKV_REST_TOKEN=capy_kv_secret",
		"CAPYKV_REDIS_URL=rediss://default:capy_kv_secret@kv-app-abc123.db.capydb.dev:6379",
		"cannot be shown again",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("create output missing %q:\n%s", expected, out)
		}
	}
}

func TestKVCreateWriteEnvMergesEveryKey(t *testing.T) {
	projectID := "project_kv"
	server := kvTestServer(t, projectID, map[string]func(http.ResponseWriter, *http.Request){
		"POST /v1/projects/" + projectID + "/kv": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{
				"job": map[string]any{"id": "job_kv_1", "state": "pending"},
				"kv_store": map[string]any{
					"id": "kv_1", "project_id": projectID, "state": "provisioning",
					"token": "capy_kv_secret",
					"credentials": map[string]any{
						"rest_url": "https://kv-app.db.capydb.dev", "rest_token": "capy_kv_secret",
						"redis_url": "rediss://default:capy_kv_secret@kv-app.db.capydb.dev:6379",
					},
				},
			})
		},
	})
	defer server.Close()

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("EXISTING=keep-me\n"), 0o600); err != nil {
		t.Fatalf("seed env file: %v", err)
	}

	stdout, stderr, err := runKVCommandStreams(t, dir,
		"kv", "create", "--api-url", server.URL, "--api-key", "capy_test_key",
		"--project", projectID, "--write-env", "--env-file", ".env")
	if err != nil {
		t.Fatalf("execute kv create --write-env: %v\n%s%s", err, stdout, stderr)
	}

	// The token is now somewhere durable, so it must not also sit in the
	// terminal scrollback - that is a second copy nobody asked for.
	if strings.Contains(stdout+stderr, "capy_kv_secret") {
		t.Fatalf("--write-env echoed the token to the terminal:\n%s%s", stdout, stderr)
	}
	if !strings.Contains(stdout, "CAPYKV_REST_URL=https://kv-app.db.capydb.dev") {
		t.Fatalf("--write-env should still print the endpoint:\n%s", stdout)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	text := string(data)
	for _, expected := range []string{
		"EXISTING=keep-me",
		`CAPYKV_REST_URL="https://kv-app.db.capydb.dev"`,
		`CAPYKV_REST_TOKEN="capy_kv_secret"`,
		`CAPYKV_REDIS_URL="rediss://default:capy_kv_secret@kv-app.db.capydb.dev:6379"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("env file missing %q:\n%s", expected, text)
		}
	}
}

// --write-env is resolved before the store is created: a store that exists but
// whose token went nowhere is the one outcome that cannot be undone.
func TestKVCreateRefusesUnusableWriteEnvBeforeCreating(t *testing.T) {
	projectID := "project_kv"
	// The POST route is deliberately absent - the mock fails the test if the
	// command reaches it.
	server := kvTestServer(t, projectID, nil)
	defer server.Close()

	out, err := runKVCommand(t, t.TempDir(),
		"kv", "create", "--api-url", server.URL, "--api-key", "capy_test_key",
		"--project", projectID, "--write-env")
	if err == nil {
		t.Fatalf("--write-env without a link or --env-file should fail:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--write-env") {
		t.Fatalf("error should name the flag that failed: %v", err)
	}
}

// If the env write fails after the response arrives, the terminal is the only
// copy of the token left - so it is printed, and the command still errors.
func TestKVCreatePrintsTheTokenWhenTheEnvWriteFails(t *testing.T) {
	// The failure is manufactured with directory permissions, which neither
	// Windows nor root honours.
	if runtime.GOOS == "windows" {
		t.Skip("directory mode bits do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	projectID := "project_kv"
	server := kvTestServer(t, projectID, map[string]func(http.ResponseWriter, *http.Request){
		"POST /v1/projects/" + projectID + "/kv": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{
				"job": map[string]any{"id": "job_kv_1", "state": "pending"},
				"kv_store": map[string]any{
					"id": "kv_1", "project_id": projectID, "state": "provisioning",
					"token": "capy_kv_secret",
					"credentials": map[string]any{
						"rest_url": "https://kv-app.db.capydb.dev", "rest_token": "capy_kv_secret",
						"redis_url": "rediss://default:capy_kv_secret@kv-app.db.capydb.dev:6379",
					},
				},
			})
		},
	})
	defer server.Close()

	dir := t.TempDir()
	readOnly := filepath.Join(dir, "locked")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatalf("create read-only dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	out, err := runKVCommand(t, dir,
		"kv", "create", "--api-url", server.URL, "--api-key", "capy_test_key",
		"--project", projectID, "--write-env", "--env-file", filepath.Join("locked", ".env"))
	if err == nil {
		t.Fatalf("a failed env write should surface as an error:\n%s", out)
	}
	if !strings.Contains(out, "CAPYKV_REST_TOKEN=capy_kv_secret") {
		t.Fatalf("the token must still reach the operator:\n%s", out)
	}
	if !strings.Contains(out, "only copy") {
		t.Fatalf("output should say the values printed are the only copy:\n%s", out)
	}
}

// The token is unrecoverable by design. A read that quietly returned a
// password-free RESP URL would look like a working credential, so the command
// has to say why it is absent.
func TestKVCredentialsExplainsTheMissingToken(t *testing.T) {
	projectID := "project_kv"
	server := kvTestServer(t, projectID, map[string]func(http.ResponseWriter, *http.Request){
		"GET /v1/projects/" + projectID + "/kv/credentials": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{
				"rest_url":       "https://kv-app.db.capydb.dev",
				"redis_url":      "rediss://kv-app.db.capydb.dev:6379",
				"redis_host":     "kv-app.db.capydb.dev",
				"redis_port":     6379,
				"token_required": true,
			})
		},
	})
	defer server.Close()

	out, err := runKVCommand(t, t.TempDir(),
		"kv", "credentials", "--api-url", server.URL, "--api-key", "capy_test_key", "--project", projectID)
	if err != nil {
		t.Fatalf("execute kv credentials: %v\n%s", err, out)
	}
	if strings.Contains(out, "capy_kv_") {
		t.Fatalf("credentials output leaked a token:\n%s", out)
	}
	for _, expected := range []string{"CAPYKV_REST_URL=", "cannot be read back", "rotate-token"} {
		if !strings.Contains(out, expected) {
			t.Fatalf("credentials output missing %q:\n%s", expected, out)
		}
	}

	// `--output json` promises stdout carries only the document. The prose
	// explanation has no place there; `token_required` is what a machine reads
	// instead.
	stdout, stderr, err := runKVCommandStreams(t, t.TempDir(),
		"kv", "credentials", "--api-url", server.URL, "--api-key", "capy_test_key",
		"--project", projectID, "--output", "json")
	if err != nil {
		t.Fatalf("execute kv credentials --output json: %v\n%s%s", err, stdout, stderr)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("stdout is not a bare JSON document: %v\n%s", err, stdout)
	}
	if document["rest_url"] != "https://kv-app.db.capydb.dev" {
		t.Fatalf("json document missing rest_url:\n%s", stdout)
	}
	if document["token_required"] != true {
		t.Fatalf("json document should still say a token is required:\n%s", stdout)
	}
	if stderr != "" {
		t.Fatalf("json mode should leave stderr clean:\n%s", stderr)
	}
}

func TestKVStatusReportsNoStoreWithoutFailing(t *testing.T) {
	projectID := "project_kv"
	server := kvTestServer(t, projectID, map[string]func(http.ResponseWriter, *http.Request){
		"GET /v1/projects/" + projectID + "/kv": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "kv store not found"})
		},
	})
	defer server.Close()

	out, err := runKVCommand(t, t.TempDir(),
		"kv", "status", "--api-url", server.URL, "--api-key", "capy_test_key", "--project", projectID)
	if err != nil {
		t.Fatalf("a project without a K/V store is not an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "has no K/V store") {
		t.Fatalf("status output missing the empty-state sentence:\n%s", out)
	}
}

// Every destructive K/V operation is irreversible and none has a backup to
// restore from, so none may proceed unconfirmed on a non-interactive stdin.
func TestKVDestructiveCommandsRefuseWithoutConfirmation(t *testing.T) {
	projectID := "project_kv"
	for _, subcommand := range []string{"flush", "delete", "rotate-token"} {
		t.Run(subcommand, func(t *testing.T) {
			server := kvTestServer(t, projectID, map[string]func(http.ResponseWriter, *http.Request){})
			defer server.Close()

			out, err := runKVCommand(t, t.TempDir(),
				"kv", subcommand, "--api-url", server.URL, "--api-key", "capy_test_key", "--project", projectID)
			if err == nil {
				t.Fatalf("%s proceeded without confirmation:\n%s", subcommand, out)
			}
			if !strings.Contains(err.Error(), "not confirmed") {
				t.Fatalf("%s failed for the wrong reason: %v", subcommand, err)
			}
		})
	}
}

func TestKVStatusJSONOutputIsMachineReadable(t *testing.T) {
	projectID := "project_kv"
	server := kvTestServer(t, projectID, map[string]func(http.ResponseWriter, *http.Request){
		"GET /v1/projects/" + projectID + "/kv": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{
				"id": "kv_1", "project_id": projectID, "state": "running", "maxmemory_mb": 128,
			})
		},
	})
	defer server.Close()

	out, err := runKVCommand(t, t.TempDir(),
		"kv", "status", "--api-url", server.URL, "--api-key", "capy_test_key",
		"--project", projectID, "--output", "json")
	if err != nil {
		t.Fatalf("execute kv status --output json: %v\n%s", err, out)
	}
	var decoded struct {
		KVStore struct {
			ID          string `json:"id"`
			MaxMemoryMB int    `json:"maxmemory_mb"`
			State       string `json:"state"`
		} `json:"kv_store"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("kv status --output json is not JSON: %v\n%s", err, out)
	}
	if decoded.KVStore.ID != "kv_1" || decoded.KVStore.State != "running" || decoded.KVStore.MaxMemoryMB != 128 {
		t.Fatalf("unexpected decoded store: %+v", decoded.KVStore)
	}
}
