package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

func TestApplyImportPreset(t *testing.T) {
	cases := []struct {
		provider, in, want string
		changes            int
	}{
		{"neon", "postgres://u:p@ep-cool-bird-123-pooler.eu-central-1.aws.neon.tech/db?sslmode=require&channel_binding=require",
			"postgres://u:p@ep-cool-bird-123.eu-central-1.aws.neon.tech/db?sslmode=require&channel_binding=require", 1},
		{"neon", "postgres://u:p@ep-x.eu-central-1.aws.neon.tech/db", "postgres://u:p@ep-x.eu-central-1.aws.neon.tech/db?sslmode=require", 1},
		{"supabase", "postgresql://postgres.abc:p@aws-1-eu-west-1.pooler.supabase.com:6543/postgres",
			"postgresql://postgres.abc:p@aws-1-eu-west-1.pooler.supabase.com:5432/postgres?sslmode=require", 2},
		{"planetscale", "postgres://u:p@aws.connect.psdb.cloud:6432/app?sslmode=verify-full",
			"postgres://u:p@aws.connect.psdb.cloud:5432/app?sslmode=verify-full", 1},
		{"railway", "postgres://u:p@shuttle.proxy.rlwy.net:41234/railway", "postgres://u:p@shuttle.proxy.rlwy.net:41234/railway", 0},
		{"render", "postgres://u:p@dpg-abc-a.frankfurt-postgres.render.com/app", "postgres://u:p@dpg-abc-a.frankfurt-postgres.render.com/app?sslmode=require", 1},
		{"rds", "postgres://u:p@db.abc.eu-west-1.rds.amazonaws.com:5432/app?sslmode=verify-full", "postgres://u:p@db.abc.eu-west-1.rds.amazonaws.com:5432/app?sslmode=verify-full", 0},
	}
	for _, c := range cases {
		result, err := applyImportPreset(c.provider, c.in)
		if err != nil {
			t.Fatalf("%s %s: %v", c.provider, c.in, err)
		}
		if result.URL != c.want {
			t.Errorf("%s: got %s, want %s", c.provider, result.URL, c.want)
		}
		if len(result.Changes) != c.changes {
			t.Errorf("%s: changes = %v", c.provider, result.Changes)
		}
	}
}

func TestApplyImportPresetRefusesUnreachableEndpoints(t *testing.T) {
	for provider, in := range map[string]string{
		"railway": "postgres://u:p@postgres.railway.internal:5432/railway",
		"render":  "postgres://u:p@dpg-abc-a/app",
		"neon":    "host=ep-x.neon.tech user=u",
		"heroku":  "postgres://u:p@h/db",
	} {
		if _, err := applyImportPreset(provider, in); err == nil {
			t.Errorf("%s %s: accepted", provider, in)
		}
	}
}

func TestApplyImportPresetWarnsOnForeignHost(t *testing.T) {
	result, err := applyImportPreset("neon", "postgres://u:p@db.example.com/app?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], "does not look like a neon host") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

func TestImportFromNeonRewritesSourceURLBeforePreflight(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("CI", "true")
	// The preset fails before any request when the URL is not a URL.
	output, err := runCommand(t, t.TempDir(), "import", "preflight", "--from-neon", "--source-url", "host=x", "--api-key", "k", "--api-url", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "postgres:// URL") {
		t.Fatalf("err = %v\n%s", err, output)
	}
	if _, err := runCommand(t, t.TempDir(), "import", "--from", "neon", "--from-neon", "--api-key", "k"); err == nil {
		t.Fatal("--from without --source-url accepted")
	}
	if _, err := runCommand(t, t.TempDir(), "import", "--dry-run", "--source-url", "postgres://h/db", "--api-key", "k", "--api-url", "http://127.0.0.1:1"); err == nil {
		t.Fatal("--dry-run without --data-only or --from-supabase-dump accepted")
	}
}

func TestImportLocalPathsValidateFlags(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("CI", "true")
	for _, args := range [][]string{
		{"import", "--data-only"},
		{"import", "--data-only", "--source-url", "postgres://h/db", "--file", "x.dump"},
		{"import", "--data-only", "--source-url", "postgres://h/db", "--follow"},
		{"import", "--data-only", "--from-supabase-dump", "x.dump"},
		{"import", "--from-supabase-dump", "x.dump", "--file", "y.dump"},
		{"import", "--from-supabase-dump", "missing.dump"},
		{"import", "--from-supabase-dump", "missing.dump", "--uid-type", "int"},
	} {
		_, err := runCommand(t, t.TempDir(), append(args, "--api-key", "k", "--api-url", "http://127.0.0.1:1")...)
		var coded *exitcode.Error
		if !errors.As(err, &coded) || coded.Code != exitcode.UsageError {
			t.Errorf("%v: err = %v, want a usage error before any request", args, err)
		}
	}
}
