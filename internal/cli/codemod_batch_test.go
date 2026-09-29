package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteDrizzleBatchesTypeScript(t *testing.T) {
	input := `export async function load() {
  const [users, count] = await db.batch([
    db.select().from(usersTable).where(eq(usersTable.note, "db.batch stays text")),
    db.$count(postsTable, sql` + "`author = ${db.dialect}`" + `),
  ]);
  return { users, count };
}
`
	got, count, notes := rewriteDrizzleBatches(input, true)
	if count != 1 || len(notes) != 0 {
		t.Fatalf("count = %d, notes = %+v", count, notes)
	}
	want := `export async function load() {
  const [users, count] = await db.transaction(async (tx) => {
    const r0 = await tx.select().from(usersTable).where(eq(usersTable.note, "db.batch stays text"));
    const r1 = await tx.$count(postsTable, sql` + "`author = ${tx.dialect}`" + `);
    return [r0, r1] as [typeof r0, typeof r1];
  });
  return { users, count };
}
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRewriteDrizzleBatchesJavaScriptAndNameCollisions(t *testing.T) {
	input := `const out = await client.batch([client.insert(t).values({ tx: 1, r0: 2 })]);`
	got, count, _ := rewriteDrizzleBatches(input, false)
	if count != 1 {
		t.Fatalf("count = %d", count)
	}
	want := `const out = await client.transaction(async (tx1) => {
  const r1_0 = await tx1.insert(t).values({ tx: 1, r0: 2 });
  return [r1_0];
})`
	if got != want+";" {
		t.Fatalf("got:\n%s\nwant something like:\n%s", got, want)
	}
}

func TestRewriteDrizzleBatchesReportsUnsafeCallSites(t *testing.T) {
	for name, input := range map[string]string{
		"variables":     "await db.batch([first, second]);",
		"property":      "await this.db.batch([this.db.select().from(t)]);",
		"spread":        "await db.batch([...queries]);",
		"not an array":  "await db.batch(queries);",
		"await inside":  "await db.batch([db.select().from(await table())]);",
		"not drizzle":   "await db.batch([db.set('k', 1)]);",
		"regex literal": "const re = /db/; await db.batch([db.select().from(t)]);",
	} {
		got, count, notes := rewriteDrizzleBatches(input, true)
		if count != 0 || got != input {
			t.Errorf("%s: rewrote an unsafe call site:\n%s", name, got)
		}
		if len(notes) != 1 || !strings.Contains(notes[0].reason, "not rewritten") {
			t.Errorf("%s: notes = %+v", name, notes)
		}
	}
}

func TestJSCodeMaskSkipsStringsCommentsAndTemplates(t *testing.T) {
	src := "a('x)', \"y]\") // (\n/* [ */ b`t${c(1)}u`"
	mask, ok := jsCodeMask(src)
	if !ok {
		t.Fatal("mask failed")
	}
	var code strings.Builder
	for i := range src {
		if mask[i] {
			code.WriteByte(src[i])
		}
	}
	if got := code.String(); got != "a(, ) \n bc(1)" {
		t.Fatalf("code bytes = %q", got)
	}
}

func TestRunNeonCodemodRewritesBatchAcrossTheRepo(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "db.ts"), `import { neon } from "@neondatabase/serverless";
import { drizzle } from "drizzle-orm/neon-http";
export const db = drizzle({ client: neon(process.env.DATABASE_URL!) });
`)
	route := filepath.Join(dir, "route.ts")
	routeSource := `import { db } from "./db";
export const run = () => db.batch([db.delete(a), db.insert(b).values(v)]);
export const keep = () => db.batch(prepared);
`
	writeTestFile(t, route, routeSource)

	report, err := runNeonCodemod(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(route)
	if !strings.Contains(string(raw), "db.transaction(async (tx) => {") || !strings.Contains(string(raw), "await tx.delete(a);") {
		t.Fatalf("route not rewritten:\n%s", raw)
	}
	manual := 0
	for _, note := range report.Manual {
		if note.File == route && note.Line == 3 {
			manual++
		}
	}
	if manual != 1 {
		t.Fatalf("the unsafe call site on line 3 must be reported once: %+v", report.Manual)
	}
}

func TestRunNeonCodemodLeavesBatchAloneWithoutNeonHTTP(t *testing.T) {
	dir := t.TempDir()
	source := "export const run = () => db.batch([db.delete(a)]);\n"
	writeTestFile(t, filepath.Join(dir, "route.ts"), source)
	report, err := runNeonCodemod(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "route.ts"))
	if string(raw) != source || len(report.Changes) != 0 {
		t.Fatalf("a non-Neon repo (D1, libsql) must keep its batch calls:\n%s", raw)
	}
}
