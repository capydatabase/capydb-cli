package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodemodSourceFileNeonHTTP(t *testing.T) {
	input := `import { neon } from "@neondatabase/serverless";
import { drizzle } from "drizzle-orm/neon-http";
import type { NeonHttpDatabase } from "drizzle-orm/neon-http";

const sql = neon(process.env.DATABASE_URL!);
export const db: NeonHttpDatabase<typeof schema> = drizzle({ client: sql, schema });
`
	report := codemodReport{}
	got := codemodSourceFile("src/db.ts", input, &report)

	for _, want := range []string{
		`import postgres from "postgres";`,
		`import { drizzle } from "drizzle-orm/postgres-js";`,
		`import type { PostgresJsDatabase } from "drizzle-orm/postgres-js";`,
		`const sql = postgres(process.env.DATABASE_URL!, { max: 1, prepare: false });`,
		`export const db: PostgresJsDatabase = drizzle({ client: sql });`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten output missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "neon") {
		t.Errorf("rewritten output still references neon:\n%s", got)
	}
	if len(report.Manual) != 0 {
		t.Errorf("plain neon-http swap should need no manual notes, got %v", report.Manual)
	}
}

func TestCodemodSourceFileAddsMissingTypeImportAndDeduplicatesDriverImport(t *testing.T) {
	input := `import { neon } from "@neondatabase/serverless";
import type { NeonDatabase } from "@neondatabase/serverless";
const sql = neon(url);
export const db: NeonDatabase<typeof schema> = drizzle(sql, { schema });
`
	report := codemodReport{}
	got := codemodSourceFile("src/db.ts", input, &report)

	if strings.Count(got, `import postgres from "postgres";`) != 1 {
		t.Errorf("postgres default import must be emitted once:\n%s", got)
	}
	for _, want := range []string{
		`import type { PostgresJsDatabase } from "drizzle-orm/postgres-js";`,
		`export const db: PostgresJsDatabase = drizzle({ client: sql });`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten output missing %q\n---\n%s", want, got)
		}
	}
}

func TestCodemodSourceFileFlagsRelationalQueryMigration(t *testing.T) {
	input := `import { neon } from "@neondatabase/serverless";
const sql = neon(url);
const db = drizzle({ client: sql, schema });
await db.query.users.findMany();
`
	report := codemodReport{}
	got := codemodSourceFile("src/db.ts", input, &report)

	if !strings.Contains(got, `drizzle({ client: sql })`) {
		t.Errorf("legacy schema config was not removed:\n%s", got)
	}
	if len(report.Manual) == 0 || !strings.Contains(report.Manual[len(report.Manual)-1].Reason, "defineRelations") {
		t.Errorf("db.query migration must be reported, got %+v", report.Manual)
	}
}

func TestCodemodSourceFileFlagsManualPatterns(t *testing.T) {
	input := `import { Pool, neonConfig } from "@neondatabase/serverless";
import { drizzle } from "drizzle-orm/neon-serverless";
neonConfig.webSocketConstructor = ws;
const pool = new Pool({ connectionString: url });
const db = drizzle({ client: pool });
await db.batch([a, b]);
`
	report := codemodReport{}
	got := codemodSourceFile("src/db.ts", input, &report)

	if !strings.Contains(got, `import postgres from "postgres";`) {
		t.Errorf("import not swapped:\n%s", got)
	}
	if !strings.Contains(got, "drizzle-orm/postgres-js") {
		t.Errorf("adapter not swapped:\n%s", got)
	}

	reasons := make([]string, 0, len(report.Manual))
	for _, note := range report.Manual {
		reasons = append(reasons, note.Reason)
	}
	joined := strings.Join(reasons, "\n")
	for _, want := range []string{"Pool", "neonConfig"} {
		if !strings.Contains(joined, want) {
			t.Errorf("manual notes missing %q, got:\n%s", want, joined)
		}
	}
}

func TestCodemodSourceFileUntouchedWithoutNeon(t *testing.T) {
	input := `import postgres from "postgres";
const db = postgres(url);
`
	report := codemodReport{}
	if got := codemodSourceFile("src/db.ts", input, &report); got != input {
		t.Errorf("file without neon usage must be untouched")
	}
	if len(report.Changes)+len(report.Manual) != 0 {
		t.Errorf("no findings expected, got %+v", report)
	}
}

func TestDetectPoolerStartupParams(t *testing.T) {
	input := `import postgres from "postgres";
const sql = postgres(url, {
  prepare: false,
  connection: { statement_timeout: 10_000, application_name: "app" },
});
const pool = new Pool({ options: '-c search_path=tenant' });
`
	report := codemodReport{}
	detectPoolerStartupParams("src/db.ts", input, &report)

	if len(report.Changes) != 0 {
		t.Errorf("detection must be warn-only, got changes: %+v", report.Changes)
	}
	var joined strings.Builder
	for _, note := range report.Manual {
		joined.WriteString(note.Reason + "\n")
	}
	if !strings.Contains(joined.String(), "connection.statement_timeout") || !strings.Contains(joined.String(), "never applies it") {
		t.Errorf("missing ignored-GUC warning, got:\n%s", joined.String())
	}
	if !strings.Contains(joined.String(), "ALTER ROLE") {
		t.Errorf("warning must carry the ALTER ROLE remediation, got:\n%s", joined.String())
	}
	if !strings.Contains(joined.String(), "-c search_path") {
		t.Errorf("missing libpq options warning, got:\n%s", joined.String())
	}
	if strings.Contains(joined.String(), "application_name") {
		t.Errorf("application_name is pooler-tracked and must not warn, got:\n%s", joined.String())
	}
}

func TestDetectPoolerStartupParamsRejectedGUC(t *testing.T) {
	input := `const sql = postgres(url, { connection: { search_path: "tenant_42" } });`
	report := codemodReport{}
	detectPoolerStartupParams("src/db.ts", input, &report)
	if len(report.Manual) != 1 {
		t.Fatalf("want 1 note, got %+v", report.Manual)
	}
	if !strings.Contains(report.Manual[0].Reason, "08P01") {
		t.Errorf("rejected GUC must mention the handshake failure, got: %s", report.Manual[0].Reason)
	}
}

func TestDetectPoolerStartupParamsEnvDSN(t *testing.T) {
	input := "DATABASE_URL=postgresql://u:p@x.db.capydb.dev:6432/db?sslmode=require&options=-c%20statement_timeout=5000\n"
	report := codemodReport{}
	detectPoolerStartupParams(".env", input, &report)
	if len(report.Manual) != 1 || !strings.Contains(report.Manual[0].Reason, "statement_timeout") {
		t.Errorf("DSN options GUC not flagged: %+v", report.Manual)
	}
}

func TestSwapNeonPackageJSON(t *testing.T) {
	input := `{
  "dependencies": {
    "@neondatabase/serverless": "^0.10.0",
    "drizzle-orm": "^0.44.0"
  }
}
`
	got, changed := swapNeonPackageJSON(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "@neondatabase/serverless") {
		t.Errorf("neon dependency not removed:\n%s", got)
	}
	if !strings.Contains(got, `"postgres": "^3.4.9"`) {
		t.Errorf("postgres dependency not added:\n%s", got)
	}

	if _, changed := swapNeonPackageJSON(got); changed {
		t.Error("codemod must be idempotent on package.json")
	}
}

func TestCodemodDrizzleConfig(t *testing.T) {
	input := `export default defineConfig({
  dbCredentials: { url: process.env.DATABASE_URL! },
});
`
	report := codemodReport{}
	got := codemodDrizzleConfig("drizzle.config.ts", input, &report)
	if !strings.Contains(got, "process.env.DATABASE_DIRECT_URL ?? process.env.DATABASE_URL!") {
		t.Errorf("drizzle config url not rewritten:\n%s", got)
	}
	// Idempotent: a config already preferring the direct URL is untouched.
	report = codemodReport{}
	if again := codemodDrizzleConfig("drizzle.config.ts", got, &report); again != got {
		t.Error("drizzle config codemod must be idempotent")
	}
}

func TestCodemodEnvFile(t *testing.T) {
	input := "DATABASE_URL=postgresql://user:pass@ep-x.neon.tech/db?sslmode=require&channel_binding=require\n"
	report := codemodReport{}
	got := codemodEnvFile(".env", input, &report)
	if strings.Contains(got, "channel_binding") {
		t.Errorf("channel_binding not stripped:\n%s", got)
	}
	if !strings.Contains(got, "?sslmode=require\n") {
		t.Errorf("unexpected env rewrite:\n%s", got)
	}
	if len(report.Manual) == 0 || !strings.Contains(report.Manual[0].Reason, "Neon connection string") {
		t.Errorf("neon.tech URL must be flagged for manual attention, got %+v", report.Manual)
	}
}

func TestRunNeonCodemodDryRunAndWrite(t *testing.T) {
	dir := t.TempDir()
	source := `import { neon } from "@neondatabase/serverless";
const sql = neon(url);
`
	sourcePath := filepath.Join(dir, "db.ts")
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "@neondatabase"), 0o755); err != nil {
		t.Fatal(err)
	}
	skipped := filepath.Join(dir, "node_modules", "@neondatabase", "index.ts")
	if err := os.WriteFile(skipped, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := runNeonCodemod(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changes) == 0 {
		t.Fatal("dry run found no changes")
	}
	raw, _ := os.ReadFile(sourcePath)
	if string(raw) != source {
		t.Error("dry run must not modify files")
	}

	if _, err := runNeonCodemod(dir, true); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(sourcePath)
	if !strings.Contains(string(raw), "postgres(url, { max: 1, prepare: false })") {
		t.Errorf("write run did not apply rewrite:\n%s", raw)
	}
	rawSkipped, _ := os.ReadFile(skipped)
	if string(rawSkipped) != source {
		t.Error("node_modules must be skipped")
	}
}
