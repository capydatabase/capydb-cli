package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestOrderByForeignKeys(t *testing.T) {
	tables := []string{"public.a_children", "public.m_self", "public.z_parents", "public.b_grandchildren"}
	edges := []fkEdge{
		{child: "public.a_children", parent: "public.z_parents"},
		{child: "public.b_grandchildren", parent: "public.a_children"},
		{child: "public.m_self", parent: "public.m_self"},
		{child: "public.a_children", parent: "public.outside"},
	}
	order, deferred, err := orderByForeignKeys(tables, edges)
	if err != nil || deferred {
		t.Fatalf("err = %v, deferred = %v", err, deferred)
	}
	if got := strings.Join(order, ","); got != "public.m_self,public.z_parents,public.a_children,public.b_grandchildren" {
		t.Fatalf("order = %s", got)
	}

	cycle := []fkEdge{{child: "public.x", parent: "public.y"}, {child: "public.y", parent: "public.x", deferrable: true}}
	order, deferred, err = orderByForeignKeys([]string{"public.x", "public.y"}, cycle)
	if err != nil || !deferred || strings.Join(order, ",") != "public.y,public.x" {
		t.Fatalf("deferrable cycle: order = %v, deferred = %v, err = %v", order, deferred, err)
	}

	cycle[1].deferrable = false
	if _, _, err := orderByForeignKeys([]string{"public.x", "public.y"}, cycle); err == nil || !strings.Contains(err.Error(), "public.x, public.y") {
		t.Fatalf("hard cycle: err = %v", err)
	}
}

// liveTenantURL creates a non-superuser role that owns a fresh database -
// the shape of a CapyDB project credential - and returns its URL.
func liveTenantURL(t *testing.T) string {
	t.Helper()
	adminDB := liveDatabaseURL(t)
	parsed, err := url.Parse(adminDB)
	if err != nil {
		t.Fatal(err)
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	role := database + "_owner"
	liveExec(t, adminDB, fmt.Sprintf(`CREATE ROLE %[1]s LOGIN PASSWORD 'pw'; ALTER DATABASE %[2]s OWNER TO %[1]s; ALTER SCHEMA public OWNER TO %[1]s; CREATE EXTENSION IF NOT EXISTS "uuid-ossp";`, role, database))
	t.Cleanup(func() {
		admin := strings.Replace(adminDB, "/"+database, "/postgres", 1)
		conn, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		_, _ = conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	parsed.User = url.UserPassword(role, "pw")
	return parsed.String()
}

func TestDataOnlyImportAgainstPostgres(t *testing.T) {
	sourceURL := liveDatabaseURL(t)
	targetURL := liveTenantURL(t)

	const schemaSQL = `
CREATE TABLE z_parents (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name text NOT NULL);
CREATE TABLE a_children (id serial PRIMARY KEY, parent_id bigint NOT NULL REFERENCES z_parents, note text);
CREATE TABLE tree (id int PRIMARY KEY, parent_id int REFERENCES tree);
CREATE TABLE x (id int PRIMARY KEY, y_id int);
CREATE TABLE y (id int PRIMARY KEY, x_id int REFERENCES x DEFERRABLE);
ALTER TABLE x ADD FOREIGN KEY (y_id) REFERENCES y;
CREATE TABLE events (id int, at date NOT NULL, amount int, doubled int GENERATED ALWAYS AS (amount * 2) STORED) PARTITION BY RANGE (at);
CREATE TABLE events_2026 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE secrets (id int PRIMARY KEY, owner text);
`
	liveExec(t, sourceURL, schemaSQL+`
ALTER TABLE a_children ADD COLUMN legacy text;
INSERT INTO z_parents (name) SELECT 'p' || g FROM generate_series(1, 5) g;
INSERT INTO a_children (parent_id, note) SELECT (g % 5) + 1, 'c' || g FROM generate_series(1, 20) g;
INSERT INTO tree VALUES (2, NULL), (1, 2), (3, 1);
INSERT INTO x VALUES (1, NULL); INSERT INTO y VALUES (1, 1); UPDATE x SET y_id = 1;
INSERT INTO events (id, at, amount) VALUES (1, '2026-03-01', 10), (2, '2026-04-01', 20);
INSERT INTO secrets VALUES (1, 'alice');
`)
	liveExec(t, targetURL, schemaSQL+`
ALTER TABLE a_children ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE TABLE audit (msg text);
CREATE FUNCTION log_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO audit VALUES ('insert'); RETURN NEW; END $$;
CREATE TRIGGER parents_audit AFTER INSERT ON z_parents FOR EACH ROW EXECUTE FUNCTION log_insert();
ALTER TABLE secrets ENABLE ROW LEVEL SECURITY; ALTER TABLE secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY nobody ON secrets USING (false);
CREATE MATERIALIZED VIEW parent_count AS SELECT count(*) AS n FROM z_parents;
`)

	schema := api.DatabaseSchema{Schemas: []api.SchemaNamespace{{Name: "public", Tables: []api.SchemaTable{
		{Name: "a_children", Kind: "table", Columns: []api.SchemaColumn{{Name: "id"}, {Name: "parent_id"}, {Name: "note", IsNullable: true}, {Name: "created_at", Default: "now()"}}},
		{Name: "audit", Kind: "table", Columns: []api.SchemaColumn{{Name: "msg", IsNullable: true}}},
		{Name: "events", Kind: "partitioned_table", Columns: []api.SchemaColumn{{Name: "id", IsNullable: true}, {Name: "at"}, {Name: "amount", IsNullable: true}, {Name: "doubled", IsGenerated: true}}},
		{Name: "parent_count", Kind: "materialized_view", Columns: []api.SchemaColumn{{Name: "n"}}},
		{Name: "secrets", Kind: "table", Columns: []api.SchemaColumn{{Name: "id"}, {Name: "owner", IsNullable: true}}},
		{Name: "tree", Kind: "table", Columns: []api.SchemaColumn{{Name: "id"}, {Name: "parent_id", IsNullable: true}}},
		{Name: "x", Kind: "table", Columns: []api.SchemaColumn{{Name: "id"}, {Name: "y_id", IsNullable: true}}},
		{Name: "y", Kind: "table", Columns: []api.SchemaColumn{{Name: "id"}, {Name: "x_id", IsNullable: true}}},
		{Name: "z_parents", Kind: "table", Columns: []api.SchemaColumn{{Name: "id", Identity: "always"}, {Name: "name"}}},
	}}}}

	ctx := context.Background()
	src, err := pgx.Connect(ctx, sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close(ctx) }()
	dst, err := pgx.Connect(ctx, targetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Close(ctx) }()

	plan, err := planDataOnlyImport(ctx, src, dst, schema)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	order := make([]string, len(plan.Tables))
	for i, table := range plan.Tables {
		order[i] = table.Name
	}
	if strings.Index(strings.Join(order, ","), "z_parents") > strings.Index(strings.Join(order, ","), "a_children") || !plan.DeferConstraints {
		t.Fatalf("order = %v, defer = %v", order, plan.DeferConstraints)
	}
	notes := strings.Join(plan.Notes, "\n")
	for _, want := range []string{"audit does not exist in the source", "legacy", "created_at is not in the source"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes missing %q:\n%s", want, notes)
		}
	}

	progress := new(bytes.Buffer)
	if err := runDataOnlyCopy(ctx, src, dst, &plan, progress); err != nil {
		t.Fatalf("copy: %v\n%s", err, progress.String())
	}

	check := liveQueryRows(t, targetURL, `SELECT
  (SELECT count(*) FROM z_parents) AS parents,
  (SELECT count(*) FROM a_children) AS children,
  (SELECT count(*) FROM tree) AS tree,
  (SELECT count(*) FROM events_2026) AS events,
  (SELECT sum(doubled) FROM events) AS doubled,
  (SELECT count(*) FROM audit) AS audit,
  (SELECT n FROM parent_count) AS mv,
  (SELECT relforcerowsecurity FROM pg_class WHERE relname = 'secrets') AS forced,
  (SELECT bool_and(tgenabled = 'O') FROM pg_trigger WHERE tgname = 'parents_audit') AS trigger_on`)
	row := check[0]
	for key, want := range map[string]any{"parents": int64(5), "children": int64(20), "tree": int64(3), "events": int64(2), "audit": int64(0), "mv": int64(5), "forced": true, "trigger_on": true} {
		if row[key] != want {
			t.Errorf("%s = %v, want %v", key, row[key], want)
		}
	}
	// The next identity and serial values continue after the source's.
	liveExec(t, targetURL, "INSERT INTO z_parents (name) VALUES ('next'); INSERT INTO a_children (parent_id) VALUES (1);")
	next := liveQueryRows(t, targetURL, "SELECT (SELECT max(id) FROM z_parents) AS parent_id, (SELECT max(id) FROM a_children) AS child_id")[0]
	if next["parent_id"] != int64(6) || next["child_id"] != int32(21) {
		t.Errorf("sequences not synced: %v", next)
	}

	// A second run refuses: the tables are no longer empty.
	if _, err := planDataOnlyImport(ctx, src, dst, schema); err == nil || !strings.Contains(err.Error(), "already has rows") {
		t.Fatalf("second plan: %v", err)
	}
}

func TestDataOnlyImportRollsBackOnFailure(t *testing.T) {
	sourceURL := liveDatabaseURL(t)
	targetURL := liveTenantURL(t)
	liveExec(t, sourceURL, "CREATE TABLE a (id int PRIMARY KEY); CREATE TABLE b (id int PRIMARY KEY, v text); INSERT INTO a VALUES (1); INSERT INTO b VALUES (1, 'too long for the target');")
	liveExec(t, targetURL, "CREATE TABLE a (id int PRIMARY KEY); CREATE TABLE b (id int PRIMARY KEY, v varchar(3));")
	schema := api.DatabaseSchema{Schemas: []api.SchemaNamespace{{Name: "public", Tables: []api.SchemaTable{
		{Name: "a", Kind: "table", Columns: []api.SchemaColumn{{Name: "id"}}},
		{Name: "b", Kind: "table", Columns: []api.SchemaColumn{{Name: "id"}, {Name: "v", IsNullable: true}}},
	}}}}
	ctx := context.Background()
	src, _ := pgx.Connect(ctx, sourceURL)
	defer func() { _ = src.Close(ctx) }()
	dst, _ := pgx.Connect(ctx, targetURL)
	defer func() { _ = dst.Close(ctx) }()
	plan, err := planDataOnlyImport(ctx, src, dst, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := runDataOnlyCopy(ctx, src, dst, &plan, new(bytes.Buffer)); err == nil {
		t.Fatal("expected the varchar(3) overflow to fail the copy")
	}
	if rows := liveQueryRows(t, targetURL, "SELECT count(*) AS n FROM a"); rows[0]["n"] != int64(0) {
		t.Fatalf("table a kept %v rows after the rollback", rows[0]["n"])
	}
}
