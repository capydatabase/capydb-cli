package cli

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	capyrls "github.com/capydatabase/capyrls"
)

func TestParseTOC(t *testing.T) {
	listing := `;
; Archive created at 2026-09-29
;
215; 1259 16390 TABLE public profiles postgres
3456; 2606 16500 FK CONSTRAINT public profiles profiles_user_id_fkey postgres
3500; 0 16390 TABLE DATA public profiles postgres
3501; 0 0 SEQUENCE SET public posts_id_seq postgres
12; 2615 2200 SCHEMA - analytics postgres
13; 3079 16400 EXTENSION - uuid-ossp
3600; 0 0 ACL public TABLE profiles postgres
3601; 3256 16600 POLICY public profiles own profile postgres
3602; 6104 16700 PUBLICATION - supabase_realtime postgres
3603; 6106 16701 PUBLICATION TABLE public supabase_realtime posts postgres
3604; 1259 16800 TABLE auth users supabase_auth_admin
`
	entries := parseTOC(listing)
	if len(entries) != 11 {
		t.Fatalf("entries = %d", len(entries))
	}
	fk := entries[1]
	if fk.kind != "FK CONSTRAINT" || fk.schema != "public" || fk.name != "profiles profiles_user_id_fkey" {
		t.Fatalf("fk entry = %+v", fk)
	}
	if entries[3].kind != "SEQUENCE SET" || entries[4].kind != "SCHEMA" || entries[4].name != "analytics" {
		t.Fatalf("entries = %+v", entries[3:5])
	}

	plan := planSupabaseRestore(entries)
	if strings.Join(plan.AppSchemas, ",") != "analytics,public" {
		t.Fatalf("schemas = %v", plan.AppSchemas)
	}
	if plan.KeptCount != 4 {
		t.Fatalf("kept = %d: %+v", plan.KeptCount, plan.Kept)
	}
	if strings.Join(plan.DumpExtensions, ",") != "uuid-ossp" {
		t.Fatalf("extensions = %v", plan.DumpExtensions)
	}
}

func TestManagedForeignKeys(t *testing.T) {
	sql := `ALTER TABLE ONLY public.profiles
    ADD CONSTRAINT profiles_user_id_fkey FOREIGN KEY (user_id) REFERENCES auth.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY "public"."Posts"
    ADD CONSTRAINT "Posts_profile_fkey" FOREIGN KEY (profile_id) REFERENCES public.profiles(id);`
	found := managedForeignKeys(sql)
	if len(found) != 1 || found["public profiles profiles_user_id_fkey"] == "" {
		t.Fatalf("found = %v", found)
	}
}

// TestSupabaseDumpRestoreAgainstPostgres builds a small Supabase-shaped
// database (auth schema, extensions schema, RLS policy, grants, publication),
// dumps it with pg_dump -Fc, and restores it as a non-superuser owner.
func TestSupabaseDumpRestoreAgainstPostgres(t *testing.T) {
	sourceURL := liveDatabaseURL(t)
	targetURL := liveTenantURL(t)
	for _, tool := range []string{"pg_dump", "pg_restore", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	liveExec(t, sourceURL, `
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'anon') THEN CREATE ROLE anon NOLOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'authenticated') THEN CREATE ROLE authenticated NOLOGIN; END IF;
END $$;
CREATE SCHEMA auth;
CREATE TABLE auth.users (id uuid PRIMARY KEY, email text);
CREATE FUNCTION auth.uid() RETURNS uuid LANGUAGE sql STABLE AS $f$ SELECT nullif(current_setting('request.jwt.claim.sub', true), '')::uuid $f$;
CREATE SCHEMA extensions;
CREATE EXTENSION "uuid-ossp" WITH SCHEMA extensions;
CREATE SCHEMA analytics;
CREATE TABLE public.profiles (id uuid PRIMARY KEY DEFAULT extensions.uuid_generate_v4(), user_id uuid NOT NULL DEFAULT auth.uid() REFERENCES auth.users (id), bio text);
CREATE TABLE public.posts (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, profile_id uuid NOT NULL REFERENCES public.profiles (id), body text);
CREATE VIEW analytics.post_counts AS SELECT profile_id, count(*) AS n FROM public.posts GROUP BY 1;
ALTER TABLE public.profiles ENABLE ROW LEVEL SECURITY;
CREATE POLICY "own profile" ON public.profiles FOR SELECT TO authenticated USING (user_id = auth.uid());
GRANT SELECT ON public.profiles TO anon, authenticated;
CREATE PUBLICATION supabase_realtime FOR TABLE public.posts;
INSERT INTO auth.users VALUES ('00000000-0000-0000-0000-000000000001', 'a@b');
INSERT INTO public.profiles (id, user_id, bio) VALUES ('00000000-0000-0000-0000-0000000000aa', '00000000-0000-0000-0000-000000000001', 'hi');
INSERT INTO public.posts (profile_id, body) VALUES ('00000000-0000-0000-0000-0000000000aa', 'first'), ('00000000-0000-0000-0000-0000000000aa', 'second');
`)
	dir := t.TempDir()
	dump := filepath.Join(dir, "supabase.dump")
	if out, err := exec.Command("pg_dump", "-Fc", "-f", dump, sourceURL).CombinedOutput(); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, out)
	}

	ctx := context.Background()
	target, err := pgx.Connect(ctx, targetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close(ctx) }()

	workDir := t.TempDir()
	plan, _, files, err := prepareSupabaseRestore(ctx, dump, workDir, target, capyrls.Options{Mode: capyrls.ModeCompat, RoleModel: capyrls.RoleSingle})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(plan.DroppedAuthFKs) != 1 || !strings.Contains(plan.DroppedAuthFKs[0], "-> auth.users") {
		t.Fatalf("dropped FKs = %v", plan.DroppedAuthFKs)
	}
	if !plan.RewriteExtensions || len(plan.MissingExtensions) != 0 {
		t.Fatalf("extensions: rewrite = %v, missing = %v", plan.RewriteExtensions, plan.MissingExtensions)
	}

	var out bytes.Buffer
	if err := applySupabaseRestore(ctx, &out, &out, targetURL, files); err != nil {
		t.Fatalf("restore: %v\n%s", err, out.String())
	}
	// FORCE ROW LEVEL SECURITY now applies to the owner too, so the counts
	// are read with it lifted inside a rolled-back transaction.
	var profiles int64
	func() {
		tx, err := target.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "ALTER TABLE public.profiles NO FORCE ROW LEVEL SECURITY"); err != nil {
			t.Fatal(err)
		}
		// The rewritten default resolves to the project's public.uuid_generate_v4().
		if _, err := tx.Exec(ctx, "INSERT INTO public.profiles (user_id) VALUES ('00000000-0000-0000-0000-000000000002')"); err != nil {
			t.Fatalf("insert with the rewritten default: %v", err)
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.profiles").Scan(&profiles); err != nil {
			t.Fatal(err)
		}
	}()
	if profiles != 2 {
		t.Errorf("profiles = %d, want 1 restored + 1 inserted", profiles)
	}
	row := liveQueryRows(t, targetURL, `SELECT
  (SELECT count(*) FROM public.posts) AS posts,
  (SELECT sum(n) FROM analytics.post_counts)::int AS counted,
  (SELECT count(*) FROM pg_policies WHERE tablename = 'profiles') > 0 AS policies,
  (SELECT relforcerowsecurity FROM pg_class WHERE oid = 'public.profiles'::regclass) AS forced,
  (SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.posts'::regclass AND contype = 'f') AS post_fks,
  (SELECT count(*) FROM pg_publication) AS publications`)[0]
	for key, want := range map[string]any{"posts": int64(2), "counted": int32(2), "policies": true, "forced": true, "post_fks": int64(1), "publications": int64(0)} {
		if row[key] != want {
			t.Errorf("%s = %v, want %v", key, row[key], want)
		}
	}
	// The identity continues after the dump's rows.
	liveExec(t, targetURL, `INSERT INTO public.posts (profile_id, body) VALUES ('00000000-0000-0000-0000-0000000000aa', 'third');`)
	if next := liveQueryRows(t, targetURL, "SELECT max(id) AS id FROM public.posts")[0]["id"]; next != int64(3) {
		t.Errorf("next post id = %v", next)
	}

	// A second restore into the now-populated project is refused by the
	// command (checked there); applying the same files again fails and
	// changes nothing.
	if err := applySupabaseRestore(ctx, &out, &out, targetURL, files); err == nil {
		t.Fatal("a second restore must fail")
	}
}
