package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// majorUpgradeServer is a control plane for the major-upgrade steps. It
// records which step endpoints were called and with which query, and answers
// the preflight with the given verdict.
func majorUpgradeServer(t *testing.T, verdict string, calls map[string]string) string {
	t.Helper()
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			calls[name] = r.URL.RawQuery
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_" + name, "state": "queued", "type": "project.upgrade_major"}})
		}
	}
	server := newFakeControlPlane(t, map[string]any{"postgres_version": "17", "postgres_channel": "stable"}, map[string]http.HandlerFunc{
		"GET /v1/me": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"organization": map[string]any{"id": "org_1", "slug": "acme"}, "principal": map[string]any{"organization_id": "org_1"}})
		},
		"POST /v1/projects/prj_1/upgrade/major/preflight": record("preflight"),
		"GET /v1/jobs/job_preflight": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"job": map[string]any{
				"id": "job_preflight", "state": "completed", "type": "project.upgrade_major_preflight",
				"result": map[string]any{"status": verdict, "current_major": "17", "target_major": "18", "blockers": []string{}, "warnings": []string{}},
			}})
		},
		"POST /v1/projects/prj_1/upgrade/major":          record("upgrade"),
		"POST /v1/projects/prj_1/upgrade/major/confirm":  record("confirm"),
		"POST /v1/projects/prj_1/upgrade/major/rollback": record("rollback"),
	})
	return server.URL
}

func TestUpgradeMajorNeedsAnApprovalBeforeThePreflight(t *testing.T) {
	t.Setenv("CAPYDB_APPROVAL_TOKEN", "")
	calls := map[string]string{}
	url := majorUpgradeServer(t, "upgradable", calls)
	output, err := runCommand(t, t.TempDir(), "upgrade", "major", "--target-major", "18", "--project", "prj_1", "--confirm", "--api-url", url, "--api-key", "capy_test")
	if err == nil {
		t.Fatalf("expected an error without an approval:\n%s", output)
	}
	if len(calls) != 0 {
		t.Fatalf("endpoints called without an approval: %v", calls)
	}
	for _, want := range []string{"needs an approval", "/settings", "--approval-token", "a major upgrade", "Nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestUpgradeMajorRunsThePreflightThenUpgrades(t *testing.T) {
	calls := map[string]string{}
	url := majorUpgradeServer(t, "upgradable", calls)
	output, err := runCommand(t, t.TempDir(), "upgrade", "major", "--target-major", "18", "--project", "prj_1", "--confirm", "--approval-token", "apv_1", "--api-url", url, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("upgrade major: %v\n%s", err, output)
	}
	if calls["preflight"] != "target_major=18" {
		t.Fatalf("preflight query = %q", calls["preflight"])
	}
	if calls["upgrade"] != "approval_token=apv_1&target_major=18" {
		t.Fatalf("upgrade query = %q", calls["upgrade"])
	}
	for _, want := range []string{"verdict: upgradable", "Queued major upgrade to Postgres 18", "capydb upgrade status"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestUpgradeMajorStopsOnABlockedPreflight(t *testing.T) {
	calls := map[string]string{}
	url := majorUpgradeServer(t, "blocked", calls)
	t.Setenv("CAPYDB_APPROVAL_TOKEN", "apv_env")
	output, err := runCommand(t, t.TempDir(), "upgrade", "major", "--target-major", "18", "--project", "prj_1", "--confirm", "--api-url", url, "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "did not pass (blocked)") {
		t.Fatalf("expected a blocked preflight error, got %v\n%s", err, output)
	}
	if _, called := calls["upgrade"]; called {
		t.Fatal("upgrade endpoint called after a blocked preflight")
	}
}

func TestUpgradeConfirmAndRollbackPresentTheirApproval(t *testing.T) {
	for _, step := range []string{"confirm", "rollback"} {
		calls := map[string]string{}
		url := majorUpgradeServer(t, "upgradable", calls)
		output, err := runCommand(t, t.TempDir(), "upgrade", step, "--project", "prj_1", "--confirm", "--approval-token", "apv_"+step, "--api-url", url, "--api-key", "capy_test")
		if err != nil {
			t.Fatalf("upgrade %s: %v\n%s", step, err, output)
		}
		if calls[step] != "approval_token=apv_"+step {
			t.Fatalf("%s query = %q", step, calls[step])
		}
		if _, preflight := calls["preflight"]; preflight {
			t.Fatalf("%s ran a preflight", step)
		}
	}
}

func TestUpgradeMajorExplainsA403(t *testing.T) {
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/upgrade/major/confirm": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(t, w, map[string]any{"error": "self-serve major upgrades are not enabled"})
		},
	})
	_, err := runCommand(t, t.TempDir(), "upgrade", "confirm", "--project", "prj_1", "--confirm", "--approval-token", "apv", "--api-url", server.URL, "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "need CapyDB to have enabled them") {
		t.Fatalf("403 error = %v", err)
	}
}

func TestUpgradeStatus(t *testing.T) {
	upgrade := map[string]any{"upgrade": nil}
	server := newFakeControlPlane(t, map[string]any{"postgres_version": "18", "postgres_channel": "current"}, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/upgrade/major": func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, upgrade) },
	})
	args := []string{"upgrade", "status", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test"}

	output, err := runCommand(t, t.TempDir(), args...)
	if err != nil || !strings.Contains(output, "No major upgrade in flight for project demo (Postgres 18 (current))") {
		t.Fatalf("status without upgrade: %v\n%s", err, output)
	}

	upgrade["upgrade"] = map[string]any{
		"from_major": "17", "to_major": "18", "state": "rollback_available",
		"rollback_available_until": "2026-10-03T10:00:00Z", "created_at": "2026-09-30T10:00:00Z", "updated_at": "2026-09-30T10:30:00Z",
	}
	output, err = runCommand(t, t.TempDir(), args...)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, output)
	}
	for _, want := range []string{"postgresql: 17 -> 18", "state: rollback_available", "rollback_available_until:", "capydb upgrade confirm"} {
		if !strings.Contains(output, want) {
			t.Fatalf("status missing %q:\n%s", want, output)
		}
	}

	output, err = runCommand(t, t.TempDir(), append(args, "-o", "json")...)
	if err != nil {
		t.Fatalf("status json: %v", err)
	}
	var payload map[string]map[string]any
	if err := json.Unmarshal([]byte(output), &payload); err != nil || payload["upgrade"]["state"] != "rollback_available" {
		t.Fatalf("status json = %s (%v)", output, err)
	}
}
