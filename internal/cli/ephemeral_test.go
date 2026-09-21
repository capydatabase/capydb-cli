package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capydatabase/capydb-cli/internal/config"
)

const (
	testEphemeralProjectID = "prj_eph123"
	testEphemeralToken     = "eph_s3cret"
	testEphemeralDirectURL = "postgres://usr:pw@ephemeral-abc.db.capydb.dev:5432/db?sslmode=verify-full"
	testEphemeralPooledURL = "postgres://usr:pw@ephemeral-abc.db.capydb.dev:6432/db?sslmode=verify-full"
)

func ephemeralDatabaseJSON(state string, expiresAt time.Time) map[string]any {
	return map[string]any{
		"created_at":       expiresAt.Add(-72 * time.Hour).Format(time.RFC3339),
		"expires_at":       expiresAt.Format(time.RFC3339),
		"name":             "ephemeral-abc",
		"postgres_version": "17",
		"project_id":       testEphemeralProjectID,
		"region":           "eu-central",
		"state":            state,
	}
}

// The whole point of the command is that it needs no account: every request must go out without an
// Authorization header, the claim token must travel in its header (never the URL), and the secrets
// must end up in files - the env file and the 0600 state file - not on stdout.
func TestEphemeralCreateIsAnonymousAndKeepsSecretsInFiles(t *testing.T) {
	isolateUserConfig(t)
	cwd := t.TempDir()
	expiresAt := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)

	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("ephemeral request carried a credential: %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ephemeral-databases":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			if request["name"] != "scratch" {
				t.Fatalf("unexpected create request: %#v", request)
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"claim_token":        testEphemeralToken,
				"claim_url":          "https://capydb.dev/dashboard/claim/" + testEphemeralProjectID + "?token=" + testEphemeralToken,
				"ephemeral_database": ephemeralDatabaseJSON("provisioning", expiresAt),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/ephemeral-databases/"+testEphemeralProjectID:
			if got := r.Header.Get("X-CapyDB-Claim-Token"); got != testEphemeralToken {
				t.Fatalf("claim token header = %q, want the token", got)
			}
			if r.URL.RawQuery != "" {
				t.Fatalf("claim token must not travel in the URL, got query %q", r.URL.RawQuery)
			}
			polls++
			writeJSON(t, w, map[string]any{
				"connections":        map[string]any{"direct_url": testEphemeralDirectURL, "pooled_url": testEphemeralPooledURL, "username": "usr"},
				"ephemeral_database": ephemeralDatabaseJSON("ready", expiresAt),
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, cwd, "--api-url", server.URL, "ephemeral", "create", "--name", "scratch")
	if err != nil {
		t.Fatalf("ephemeral create: %v\n%s", err, output)
	}
	if polls == 0 {
		t.Fatal("create returned without polling the database to ready")
	}
	if strings.Contains(output, testEphemeralToken) || strings.Contains(output, "usr:pw@") {
		t.Fatalf("create printed a secret:\n%s", output)
	}

	envData, err := os.ReadFile(filepath.Join(cwd, ".env"))
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(envData), `DATABASE_URL="`+testEphemeralDirectURL+`"`) &&
		!strings.Contains(string(envData), `DATABASE_URL="`+testEphemeralPooledURL+`"`) {
		t.Fatalf("env file has no DATABASE_URL for the ephemeral database:\n%s", envData)
	}

	state, err := config.LoadEphemeralState(cwd)
	if err != nil {
		t.Fatalf("load ephemeral state: %v", err)
	}
	if state.ClaimToken != testEphemeralToken || state.ProjectID != testEphemeralProjectID || state.EnvFile != ".env" || !state.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("unexpected ephemeral state: %+v", state)
	}
	info, err := os.Stat(config.EphemeralStatePath(cwd))
	if err != nil {
		t.Fatalf("stat ephemeral state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ephemeral state is %o, want 600: it holds the claim token", info.Mode().Perm())
	}

	gitignoreData, err := os.ReadFile(filepath.Join(cwd, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, entry := range []string{".capydb/", ".env"} {
		if !strings.Contains(string(gitignoreData), entry) {
			t.Fatalf(".gitignore is missing %q:\n%s", entry, gitignoreData)
		}
	}

	// A second create must not silently orphan the first database's claim token.
	if output, err := runCommand(t, cwd, "--api-url", server.URL, "ephemeral", "create"); err == nil {
		t.Fatalf("second create in the same directory succeeded:\n%s", output)
	}
}

func TestEphemeralCreateNoEnvPrintsConnectionStrings(t *testing.T) {
	isolateUserConfig(t)
	cwd := t.TempDir()
	expiresAt := time.Now().Add(72 * time.Hour).UTC()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ephemeral-databases":
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"claim_token":        testEphemeralToken,
				"claim_url":          "https://capydb.dev/dashboard/claim/x?token=" + testEphemeralToken,
				"ephemeral_database": ephemeralDatabaseJSON("provisioning", expiresAt),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/ephemeral-databases/"+testEphemeralProjectID:
			writeJSON(t, w, map[string]any{
				"connections":        map[string]any{"direct_url": testEphemeralDirectURL, "pooled_url": testEphemeralPooledURL, "username": "usr"},
				"ephemeral_database": ephemeralDatabaseJSON("ready", expiresAt),
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, cwd, "--api-url", server.URL, "-o", "json", "ephemeral", "create", "--no-env")
	if err != nil {
		t.Fatalf("ephemeral create --no-env: %v\n%s", err, output)
	}
	if !strings.Contains(output, testEphemeralPooledURL) {
		t.Fatalf("--no-env output has no connection string:\n%s", output)
	}
	if strings.Contains(output, testEphemeralToken) {
		t.Fatalf("--no-env output leaked the claim token:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".env")); !os.IsNotExist(err) {
		t.Fatalf("--no-env must not create an env file (stat err: %v)", err)
	}
}

func TestEphemeralClaimLinksTheDirectoryAndForgetsTheToken(t *testing.T) {
	isolateUserConfig(t)
	cwd := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("claim must be authenticated, got Authorization %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ephemeral-databases/"+testEphemeralProjectID+"/claim":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode claim request: %v", err)
			}
			if request["claim_token"] != testEphemeralToken {
				t.Fatalf("unexpected claim request: %#v", request)
			}
			writeJSON(t, w, map[string]any{"project": map[string]any{
				"id": testEphemeralProjectID, "name": "ephemeral-abc", "slug": "ephemeral-abc",
				"organization_id": "org_123", "region": "eu-central", "state": "ready",
			}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	if err := config.SaveEphemeralState(cwd, config.EphemeralState{
		APIURL:     server.URL,
		ClaimToken: testEphemeralToken,
		ExpiresAt:  time.Now().Add(time.Hour),
		Name:       "ephemeral-abc",
		ProjectID:  testEphemeralProjectID,
	}); err != nil {
		t.Fatalf("seed ephemeral state: %v", err)
	}

	output, err := runCommand(t, cwd, "--api-url", server.URL, "--api-key", "capy_test_key", "ephemeral", "claim")
	if err != nil {
		t.Fatalf("ephemeral claim: %v\n%s", err, output)
	}

	linked, err := config.LoadProjectConfig(cwd)
	if err != nil {
		t.Fatalf("claim did not link the directory: %v", err)
	}
	if linked.ProjectID != testEphemeralProjectID || linked.OrganizationID != "org_123" {
		t.Fatalf("unexpected link config: %+v", linked)
	}
	if _, err := os.Stat(config.EphemeralStatePath(cwd)); !os.IsNotExist(err) {
		t.Fatalf("the spent claim token is still on disk (stat err: %v)", err)
	}
}

func TestEphemeralStatusExplainsAGoneDatabase(t *testing.T) {
	isolateUserConfig(t)
	cwd := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"error": "resource not found"})
	}))
	defer server.Close()

	if err := config.SaveEphemeralState(cwd, config.EphemeralState{
		APIURL:     server.URL,
		ClaimToken: testEphemeralToken,
		ExpiresAt:  time.Now().Add(-time.Hour),
		Name:       "ephemeral-abc",
		ProjectID:  testEphemeralProjectID,
	}); err != nil {
		t.Fatalf("seed ephemeral state: %v", err)
	}

	output, err := runCommand(t, cwd, "ephemeral", "status")
	if err == nil {
		t.Fatalf("status of an expired database succeeded:\n%s", output)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error does not say the database expired: %v", err)
	}

	if _, err := runCommand(t, t.TempDir(), "ephemeral", "status"); err == nil || !strings.Contains(err.Error(), "capydb ephemeral create") {
		t.Fatalf("status with no record should point at create, got %v", err)
	}
}

// Destroy is anonymous like the read: the claim token in the header is the whole credential, and
// once the API has taken the database the local record goes too, so the next create is not refused.
func TestEphemeralDestroyIsAnonymousAndForgetsTheRecord(t *testing.T) {
	isolateUserConfig(t)
	cwd := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("destroy request carried a credential: %q", got)
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/ephemeral-databases/"+testEphemeralProjectID {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-CapyDB-Claim-Token"); got != testEphemeralToken {
			t.Fatalf("destroy sent claim token %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if err := config.SaveEphemeralState(cwd, config.EphemeralState{
		APIURL:     server.URL,
		ClaimToken: testEphemeralToken,
		ExpiresAt:  time.Now().Add(time.Hour),
		Name:       "ephemeral-abc",
		ProjectID:  testEphemeralProjectID,
	}); err != nil {
		t.Fatalf("seed ephemeral state: %v", err)
	}

	output, err := runCommand(t, cwd, "ephemeral", "destroy")
	if err != nil {
		t.Fatalf("ephemeral destroy: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Destroyed ephemeral database ephemeral-abc") {
		t.Fatalf("unexpected destroy output:\n%s", output)
	}
	if _, err := os.Stat(config.EphemeralStatePath(cwd)); !os.IsNotExist(err) {
		t.Fatalf("ephemeral record must be removed after destroy (stat err: %v)", err)
	}
}

// A deployment with the feature off answers the anonymous create with 404. "resource not found"
// would read as a bug in the CLI; the user needs to hear that the feature is off and what to do.
func TestEphemeralCreateExplainsADisabledDeployment(t *testing.T) {
	isolateUserConfig(t)
	cwd := t.TempDir()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"error": "resource not found"})
	}))
	defer server.Close()

	_, err := runCommand(t, cwd, "--api-url", server.URL, "ephemeral", "create", "--no-env")
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("disabled create returned %v, want an explanation that the feature is off", err)
	}
	if _, statErr := os.Stat(config.EphemeralStatePath(cwd)); !os.IsNotExist(statErr) {
		t.Fatalf("a failed create must leave no record behind (stat err: %v)", statErr)
	}
}
