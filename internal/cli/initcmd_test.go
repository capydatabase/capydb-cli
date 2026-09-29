package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDrizzleConfigTemplateTablesFilter pins the drizzle-kit table filter. The
// pg_stat_statements view is still excluded for cells that carry it in public;
// pg_stat_statements_info is not, because platform v12 revoked tenant access to
// it on every cell, so introspection no longer sees it.
func TestDrizzleConfigTemplateTablesFilter(t *testing.T) {
	if !strings.Contains(drizzleConfigTemplate, `tablesFilter: ["!pg_stat_statements"],`) {
		t.Fatalf("drizzle.config.ts template lost its pg_stat_statements filter:\n%s", drizzleConfigTemplate)
	}
	if strings.Contains(drizzleConfigTemplate, "pg_stat_statements_info") {
		t.Fatalf("drizzle.config.ts template still filters pg_stat_statements_info:\n%s", drizzleConfigTemplate)
	}
}

func TestInitPrismaScaffoldsV7Config(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, nil)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "pnpm-lock.yaml"), "")

	output, err := runCommand(t, dir, "init", "prisma", "--dir", dir, "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("init prisma: %v\n%s", err, output)
	}
	config := readTestFile(t, filepath.Join(dir, "prisma.config.ts"))
	schema := readTestFile(t, filepath.Join(dir, "prisma", "schema.prisma"))
	client := readTestFile(t, filepath.Join(dir, "src", "db.ts"))
	for _, check := range []struct{ file, content, want string }{
		{"prisma.config.ts", config, `url: process.env.DATABASE_DIRECT_URL ?? process.env.DATABASE_URL_UNPOOLED ?? env("DATABASE_URL")`},
		{"prisma.config.ts", config, `migrations: { path: "prisma/migrations" }`},
		{"schema.prisma", schema, `output   = "../src/generated/prisma"`},
		{"schema.prisma", schema, `provider = "prisma-client"`},
		{"db.ts", client, `import { PrismaClient } from "./generated/prisma/client";`},
	} {
		if !strings.Contains(check.content, check.want) {
			t.Errorf("%s is missing %q:\n%s", check.file, check.want, check.content)
		}
	}
	if strings.Contains(schema, "url") {
		t.Errorf("Prisma 7 schemas carry no datasource url:\n%s", schema)
	}
	if !strings.Contains(output, "pnpm add @prisma/client") || !strings.Contains(output, "npx prisma db pull") {
		t.Errorf("unexpected next steps:\n%s", output)
	}
}

func TestInitKyselyViaORMFlag(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var language string
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/schema/types": func(w http.ResponseWriter, r *http.Request) {
			language = r.URL.Query().Get("language")
			writeJSON(t, w, map[string]any{"types": map[string]any{"content": "export type Database = {};\n", "filename": "database.types.ts", "language": "typescript"}})
		},
	})
	dir := t.TempDir()
	output, err := runCommand(t, dir, "init", "--orm", "kysely", "--dir", dir, "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("init --orm kysely: %v\n%s", err, output)
	}
	if language != "typescript" {
		t.Fatalf("requested language %q", language)
	}
	client := readTestFile(t, filepath.Join(dir, "src", "db", "index.ts"))
	if !strings.Contains(client, `import type { Database } from "./database.types";`) || !strings.Contains(client, "export const db = new Kysely<DB>") {
		t.Fatalf("unexpected client:\n%s", client)
	}
	if _, err := runCommand(t, dir, "init", "--orm", "kysely", "--dir", dir, "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test"); err == nil {
		t.Fatal("existing files must not be overwritten without --force")
	}
	if _, err := runCommand(t, dir, "init", "--orm", "sequelize", "--api-url", server.URL, "--api-key", "capy_test"); err == nil {
		t.Fatal("unknown ORM accepted")
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
