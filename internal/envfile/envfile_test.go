package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertPreservesCommentsAndUpdatesExistingKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	input := "# existing comment\nDATABASE_URL=\"old\"\n\nNEXT_PUBLIC=true\n"
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	err := Upsert(path, map[string]string{
		"DATABASE_URL":      "postgres://new",
		"DATABASE_POOL_URL": "postgres://pooled",
	})
	if err != nil {
		t.Fatalf("upsert env: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	content := string(raw)

	if !strings.Contains(content, "# existing comment") {
		t.Fatalf("expected comment to be preserved, got:\n%s", content)
	}
	if !strings.Contains(content, "DATABASE_URL=\"postgres://new\"") {
		t.Fatalf("expected DATABASE_URL to be updated, got:\n%s", content)
	}
	if !strings.Contains(content, "DATABASE_POOL_URL=\"postgres://pooled\"") {
		t.Fatalf("expected DATABASE_POOL_URL to be appended, got:\n%s", content)
	}
}

func TestUpsertWithResolverKeepsExistingValueWhenDeclined(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("DATABASE_URL=\"hand-set\"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var prompted []string
	resolver := func(key, existing, incoming string) (bool, error) {
		prompted = append(prompted, key)
		if existing != "hand-set" {
			t.Fatalf("unexpected existing value: %q", existing)
		}
		if incoming != "postgres://new" {
			t.Fatalf("unexpected incoming value: %q", incoming)
		}
		return false, nil
	}

	if err := UpsertWithResolver(path, map[string]string{"DATABASE_URL": "postgres://new"}, resolver); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if len(prompted) != 1 || prompted[0] != "DATABASE_URL" {
		t.Fatalf("expected resolver to be invoked for DATABASE_URL, got: %v", prompted)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if !strings.Contains(string(raw), "DATABASE_URL=\"hand-set\"") {
		t.Fatalf("expected existing value to be preserved, got:\n%s", string(raw))
	}
}

func TestUpsertWithResolverSkippedWhenValueUnchanged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("DATABASE_URL=\"same\"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	resolver := func(key, existing, incoming string) (bool, error) {
		t.Fatalf("resolver should not be called when value is unchanged")
		return false, nil
	}

	if err := UpsertWithResolver(path, map[string]string{"DATABASE_URL": "same"}, resolver); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func TestValuesAndRemoveKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# db\nexport DATABASE_URL=\"postgres://old\"\nEMPTY=\nOTHER='x'\nDIRECT_URL=postgres://old2\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := Values(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["DATABASE_URL"] != "postgres://old" || values["OTHER"] != "x" || len(values) != 3 {
		t.Fatalf("values = %v", values)
	}
	removed, err := RemoveKeys(path, []string{"DATABASE_URL", "DIRECT_URL"})
	if err != nil || removed != 2 {
		t.Fatalf("removed = %d, %v", removed, err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "# db\nEMPTY=\nOTHER='x'\n" {
		t.Fatalf("file = %q", raw)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", info.Mode().Perm())
	}
	if missing, err := Values(filepath.Join(t.TempDir(), "nope")); err != nil || len(missing) != 0 {
		t.Fatalf("missing file: %v %v", missing, err)
	}
}
