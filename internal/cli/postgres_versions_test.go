package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestPostgresVersionsListsChannels(t *testing.T) {
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/postgres-versions": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"versions": []map[string]any{
				{"version": "16", "channel": "previous", "default": false, "production_ready": true},
				{"version": "17", "channel": "stable", "default": true, "production_ready": true},
				{"version": "18", "channel": "current", "default": false, "production_ready": true},
				{"version": "19", "channel": "beta", "default": false, "production_ready": false},
			}})
		},
	})

	output, err := runCommand(t, t.TempDir(), "postgres-versions", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("postgres-versions: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "CHANNEL") {
		t.Fatalf("table:\n%s", output)
	}
	if fields := strings.Fields(lines[2]); strings.Join(fields, " ") != "17 stable yes yes" {
		t.Fatalf("stable row = %q", lines[2])
	}
	if fields := strings.Fields(lines[4]); strings.Join(fields, " ") != "19 beta no no" {
		t.Fatalf("beta row = %q", lines[4])
	}

	output, err = runCommand(t, t.TempDir(), "postgres-versions", "-o", "json", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("postgres-versions json: %v\n%s", err, output)
	}
	var payload struct {
		Versions []api.PostgresVersion `json:"versions"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil || len(payload.Versions) != 4 || payload.Versions[3].Channel != "beta" {
		t.Fatalf("json = %s (%v)", output, err)
	}
}

func TestPostgresLabelAndWarning(t *testing.T) {
	if got := postgresLabel("18", "current"); got != "18 (current)" {
		t.Fatalf("postgresLabel = %q", got)
	}
	if got := postgresLabel("17", ""); got != "17" {
		t.Fatalf("postgresLabel without channel = %q", got)
	}
	if got := postgresLabel("", ""); got != "-" {
		t.Fatalf("postgresLabel empty = %q", got)
	}

	var out bytes.Buffer
	writePostgresWarning(&out, "")
	if out.Len() != 0 {
		t.Fatalf("empty warning printed %q", out.String())
	}
	writePostgresWarning(&out, "beta: not for production")
	if out.String() != "Warning: beta: not for production\n" {
		t.Fatalf("warning = %q", out.String())
	}
}

func TestCreateNotesShowChannelAndBetaWarning(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printCreateNotes(cmd, api.Project{PostgresVersion: "19", PostgresChannel: "beta", PostgresWarning: "no uptime or durability commitment"})
	for _, want := range []string{"- Postgres 19 (beta).", "- Warning: no uptime or durability commitment"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("notes missing %q:\n%s", want, out.String())
		}
	}
}
