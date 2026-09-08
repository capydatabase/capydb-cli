package scan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/capydatabase/capydbclient"
)

func readySource() *SourceFacts {
	return &SourceFacts{
		ServerVersion:     "17.4",
		DatabaseSizeBytes: 2 << 30,
		Provider:          ProviderOther,
		Replication: SourceReplication{
			WALLevel: "logical", MaxReplicationSlots: 10, MaxWALSenders: 10,
			RoleCanReplicate: true, Ready: true,
		},
		Inventory:  SourceInventory{TableCount: 40},
		Extensions: []SourceExtension{{Name: "pgcrypto", Available: true}},
	}
}

func TestAssessReadyDatabaseRecommendsStreaming(t *testing.T) {
	assessment := Assess(Report{Source: readySource()}, "test")

	if assessment.Level != LevelReady {
		t.Fatalf("level = %q, want %q (blockers: %+v, warnings: %+v)",
			assessment.Level, LevelReady, assessment.Blockers, assessment.Warnings)
	}
	if assessment.Path.ID != PathFollow {
		t.Fatalf("path = %q, want %q", assessment.Path.ID, PathFollow)
	}
	if assessment.Path.Unavailable != "" {
		t.Fatalf("a usable streaming path must not explain itself away: %q", assessment.Path.Unavailable)
	}
	if assessment.SchemaVersion != AssessmentSchemaVersion {
		t.Fatal("every assessment must stamp its schema version")
	}
}

// Heroku is the one provider where a streaming import is impossible. The
// recommendation must not merely prefer dump/restore - it must say why, or the
// reader will go looking for the flag that would have made it work.
func TestAssessHerokuCannotStream(t *testing.T) {
	source := readySource()
	source.Provider = ProviderHeroku
	assessment := Assess(Report{Source: source}, "test")

	if assessment.Path.ID != PathDumpRestore {
		t.Fatalf("path = %q, want %q", assessment.Path.ID, PathDumpRestore)
	}
	if !strings.Contains(assessment.Path.Unavailable, "Heroku") {
		t.Fatalf("the reason must name the provider, got %q", assessment.Path.Unavailable)
	}
	if strings.Contains(assessment.Path.Unavailable, "re-run") {
		t.Fatal("an impossible path must not suggest re-running the scan will fix it")
	}
}

// When the source COULD stream but is not configured for it, the fix belongs in
// the report - not a bare "not supported".
func TestAssessUnconfiguredSourceCarriesTheFix(t *testing.T) {
	source := readySource()
	source.Provider = ProviderRDS
	source.Replication = SourceReplication{
		WALLevel: "replica", MaxReplicationSlots: 10, MaxWALSenders: 10, RoleCanReplicate: true,
	}
	source.refreshReplicationReadiness()

	assessment := Assess(Report{Source: source}, "test")
	if assessment.Path.ID != PathDumpRestore {
		t.Fatalf("path = %q, want %q", assessment.Path.ID, PathDumpRestore)
	}
	for _, want := range []string{"wal_level", "rds.logical_replication", "re-run"} {
		if !strings.Contains(assessment.Path.Unavailable, want) {
			t.Errorf("explanation missing %q: %s", want, assessment.Path.Unavailable)
		}
	}
}

func TestAssessBlockersEscalateTheLevel(t *testing.T) {
	source := readySource()
	source.Extensions = append(source.Extensions,
		SourceExtension{Name: "timescaledb", Available: false, Dependents: 12})

	assessment := Assess(Report{Source: source}, "test")
	if assessment.Level != LevelAssisted {
		t.Fatalf("level = %q, want %q", assessment.Level, LevelAssisted)
	}
	if !hasFinding(assessment.Blockers, "extensions_unavailable") {
		t.Fatalf("expected the unavailable-extension blocker, got %+v", assessment.Blockers)
	}
}

// An extension nothing depends on is a note, not a blocker: it is dropped
// before the dump and never reaches the target.
func TestAssessVestigialExtensionIsOnlyANote(t *testing.T) {
	source := readySource()
	source.Extensions = append(source.Extensions,
		SourceExtension{Name: "pg_net", Available: false, Dependents: 0})

	assessment := Assess(Report{Source: source}, "test")
	if hasFinding(assessment.Blockers, "extensions_unavailable") {
		t.Fatal("an unused extension must not block the migration")
	}
	if !hasFinding(assessment.Notes, "extensions_vestigial") {
		t.Fatalf("expected the vestigial-extension note, got %+v", assessment.Notes)
	}
}

// The repository half of the scan produces findings no database probe can:
// a second service still pointing at the old database is invisible from inside
// Postgres, and it is the failure that silently splits the data in two.
func TestAssessRepoOnlyFindings(t *testing.T) {
	report := Report{
		Databases: []Database{{
			Hostname:  "db.abc.supabase.co",
			Provider:  ProviderSupabase,
			Pooled:    true,
			Consumers: []string{"../worker", "../admin"},
		}},
		EnvConflicts: []EnvConflict{{
			Key: "DATABASE_URL",
			Assignments: []EnvAssignment{
				{File: ".env", Hostname: "old.example.com"},
				{File: ".env.local", Hostname: "new.example.com"},
			},
		}},
	}
	assessment := Assess(report, "test")

	for _, id := range []string{"shared_database", "pooled_endpoint", "env_shadowing"} {
		if !hasFinding(assessment.Blockers, id) {
			t.Errorf("missing repo-only blocker %q", id)
		}
	}
	if assessment.Provider.ID != ProviderSupabase {
		t.Fatalf("a repo-only scan must still classify the provider, got %q", assessment.Provider.ID)
	}
	// With no live probe the streaming path is a guess, and must say so.
	if assessment.Path.Unavailable == "" {
		t.Fatal("an unverified recommendation must be labelled unverified")
	}
}

// What the server says beats what the hostname implied: Cloud SQL publishes no
// hostname at all, so a scan that trusted the hostname would call it
// self-managed and hand out the wrong remediation.
func TestAssessServerIdentityBeatsHostname(t *testing.T) {
	source := readySource()
	source.Provider = ProviderCloudSQL
	report := Report{
		Databases: []Database{{Hostname: "10.20.30.40", Provider: ProviderOther}},
		Source:    source,
	}
	assessment := Assess(report, "test")
	if assessment.Provider.ID != ProviderCloudSQL {
		t.Fatalf("provider = %q, want %q", assessment.Provider.ID, ProviderCloudSQL)
	}
}

func TestAssessIsJSONRoundTrippable(t *testing.T) {
	assessment := Assess(Report{Source: readySource()}, "test")
	encoded, err := json.Marshal(assessment)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The page reads this file; a nil slice arriving as `null` breaks .map().
	for _, key := range []string{`"blockers":[`, `"warnings":[`, `"notes":[`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("empty finding lists must marshal as [], not null (missing %s)", key)
		}
	}
	var decoded Assessment
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Level != assessment.Level || decoded.Provider.ID != assessment.Provider.ID {
		t.Fatal("assessment did not survive a JSON round trip")
	}
}

func TestStronglyConnectedGroups(t *testing.T) {
	// orders <-> invoices is a cycle; users -> orders is not, and the
	// self-referencing tree is excluded by the query, not here.
	edges := map[string][]string{
		"public.orders":   {"public.invoices"},
		"public.invoices": {"public.orders"},
		"public.users":    {"public.orders"},
	}
	groups := stronglyConnectedGroups(edges)
	if len(groups) != 1 {
		t.Fatalf("expected exactly one cycle, got %v", groups)
	}
	if strings.Join(groups[0], ",") != "public.invoices,public.orders" {
		t.Fatalf("cycle members wrong: %v", groups[0])
	}

	if groups := stronglyConnectedGroups(map[string][]string{"a": {"b"}, "b": {"c"}}); len(groups) != 0 {
		t.Fatalf("an acyclic graph must produce no groups, got %v", groups)
	}

	// A long chain must not recurse - the search is iterative for exactly this.
	chain := map[string][]string{}
	for index := range 5000 {
		chain[string(rune('a'+index%26))+string(rune(index))] = []string{string(rune('a'+(index+1)%26)) + string(rune(index+1))}
	}
	stronglyConnectedGroups(chain)
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:          "512 B",
		1024:         "1.0 KiB",
		1536:         "1.5 KiB",
		1 << 20:      "1.0 MiB",
		107374182400: "100.0 GiB",
		1 << 40:      "1.0 TiB",
	}
	for value, want := range cases {
		if got := humanBytes(value); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", value, got, want)
		}
	}
}

func hasFinding(findings []Finding, id string) bool {
	for _, finding := range findings {
		if finding.ID == id {
			return true
		}
	}
	return false
}

// A failing preflight check is the authoritative half of the report. It must
// re-grade the verdict, or the terminal prints "Ready to migrate" directly
// above a [fail] line.
func TestAttachPreflightEscalatesTheVerdict(t *testing.T) {
	assessment := Assess(Report{Source: readySource()}, "test")
	if assessment.Level != LevelReady {
		t.Fatalf("precondition: level = %q, want %q", assessment.Level, LevelReady)
	}

	assessment.AttachPreflight(&capydbclient.ImportPreflightResult{
		OK: false,
		Checks: []capydbclient.ImportPreflightCheck{
			{Name: "extensions_available_on_target", Status: "fail", Detail: "source uses pg_cron"},
			{Name: "no_provider_auth_policies", Status: "warn", Detail: "12 tables"},
		},
	})

	if assessment.Level != LevelAssisted {
		t.Fatalf("level = %q, want %q after a failing preflight", assessment.Level, LevelAssisted)
	}
	if !hasFinding(assessment.Blockers, "preflight_extensions_available_on_target") {
		t.Fatalf("the failing check must become a blocker, got %+v", assessment.Blockers)
	}
	// Warnings already render in the preflight block; duplicating them as
	// findings would report the same problem twice.
	if hasFinding(assessment.Warnings, "preflight_no_provider_auth_policies") {
		t.Fatal("preflight warnings must not be duplicated as findings")
	}
}

func TestAttachPreflightWithPassingChecksKeepsTheVerdict(t *testing.T) {
	assessment := Assess(Report{Source: readySource()}, "test")
	assessment.AttachPreflight(&capydbclient.ImportPreflightResult{
		OK:     true,
		Checks: []capydbclient.ImportPreflightCheck{{Name: "source_size_within_plan", Status: "pass"}},
	})
	if assessment.Level != LevelReady {
		t.Fatalf("a passing preflight must not change the verdict, got %q", assessment.Level)
	}
}

// A repo-only scan has not looked at the database. It can still find real
// blockers, but "nothing to worry about" is not something you can conclude from
// not having looked - so it must never grade `ready`.
func TestAssessNeverClearsAnUnverifiedScan(t *testing.T) {
	assessment := Assess(Report{}, "test")
	if assessment.Verified {
		t.Fatal("a scan with no live source must not be marked verified")
	}
	if assessment.Level == LevelReady {
		t.Fatal("an unverified scan must not grade ready")
	}
	if !strings.Contains(assessment.Headline, "--source-url") {
		t.Fatalf("the headline must say how to verify: %q", assessment.Headline)
	}
}
