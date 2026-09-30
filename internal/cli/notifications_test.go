package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func notificationsServer(t *testing.T, puts *[]map[string]any) string {
	t.Helper()
	current := map[string]any{
		"organization_id": "org_1", "alert_emails_enabled": true,
		"alert_email_recipients": []string{"ops@example.com"}, "billing_email_recipients": []string{"finance@example.com"},
		"updated_at": nil,
	}
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/me": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"organization": map[string]any{"id": "org_1"}, "principal": map[string]any{"organization_id": "org_1"}})
		},
		"GET /v1/organizations/org_1/notification-preferences": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"notification_preferences": current})
		},
		"PUT /v1/organizations/org_1/notification-preferences": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			*puts = append(*puts, body)
			saved := map[string]any{"organization_id": "org_1", "updated_at": "2026-09-30T10:00:00Z"}
			for key, value := range body {
				saved[key] = value
			}
			writeJSON(t, w, map[string]any{"notification_preferences": saved})
		},
	})
	return server.URL
}

func TestNotificationsShow(t *testing.T) {
	var puts []map[string]any
	url := notificationsServer(t, &puts)
	output, err := runCommand(t, t.TempDir(), "notifications", "show", "--api-url", url, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, output)
	}
	for _, want := range []string{"alert_emails: on", "alert_recipients: ops@example.com", "billing_recipients: finance@example.com", "updated_at: - (defaults)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("show missing %q:\n%s", want, output)
		}
	}
}

func TestNotificationsSetChangesOnlyWhatIsPassed(t *testing.T) {
	var puts []map[string]any
	url := notificationsServer(t, &puts)

	output, err := runCommand(t, t.TempDir(), "notifications", "set", "--alert-emails", "off", "--api-url", url, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("set: %v\n%s", err, output)
	}
	got, _ := json.Marshal(puts[0])
	if want := `{"alert_email_recipients":["ops@example.com"],"alert_emails_enabled":false,"billing_email_recipients":["finance@example.com"]}`; string(got) != want {
		t.Fatalf("put body = %s, want %s", got, want)
	}
	if !strings.Contains(output, "alert_emails: off") || !strings.Contains(output, "updated_at: 2026") {
		t.Fatalf("output:\n%s", output)
	}

	if _, err := runCommand(t, t.TempDir(), "notifications", "set", "--billing-recipients", "", "--alert-recipients", " a@example.com , b@example.com", "--api-url", url, "--api-key", "capy_test"); err != nil {
		t.Fatalf("set lists: %v", err)
	}
	got, _ = json.Marshal(puts[1])
	if want := `{"alert_email_recipients":["a@example.com","b@example.com"],"alert_emails_enabled":true,"billing_email_recipients":[]}`; string(got) != want {
		t.Fatalf("put body = %s, want %s", got, want)
	}
}

func TestNotificationsSetValidatesFlags(t *testing.T) {
	for _, args := range [][]string{
		{"notifications", "set"},
		{"notifications", "set", "--alert-emails", "maybe"},
	} {
		if _, err := runCommand(t, t.TempDir(), append(args, "--api-url", "http://127.0.0.1:1", "--api-key", "capy_test")...); err == nil {
			t.Errorf("%v: expected a usage error", args)
		}
	}
}
