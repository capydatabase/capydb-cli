package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateUserConfig redirects os.UserConfigDir into a temp directory so tests
// never read or clobber the developer's real CLI config. HOME covers macOS
// (~/Library/Application Support), XDG_CONFIG_HOME covers Linux and AppData
// covers Windows - without the last one every test on Windows shares the
// runner's real %AppData%\capydb and reads what its siblings wrote.
func isolateUserConfig(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	t.Setenv("AppData", filepath.Join(tempHome, "AppData", "Roaming"))
}

func writeUserConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path, err := UserConfigPath()
	if err != nil {
		t.Fatalf("resolve user config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write user config: %v", err)
	}
	return path
}

func TestLoadUserConfigMigratesLegacySingleKeyShape(t *testing.T) {
	isolateUserConfig(t)

	path := writeUserConfigFile(t, `{
  "api_key": "capy_legacy_key",
  "api_url": "https://capydb.dev/api/capydb",
  "app_url": "https://capydb.dev",
  "organization_id": "org_legacy",
  "organization_name": "Legacy Org",
  "organization_slug": "legacy-org"
}`)

	cfg, err := LoadUserConfig()
	if err != nil {
		t.Fatalf("load user config: %v", err)
	}

	orgID, entry, ok := cfg.Active()
	if !ok {
		t.Fatalf("expected an active organization after migration: %#v", cfg)
	}
	if orgID != "org_legacy" {
		t.Fatalf("active org = %q, want org_legacy", orgID)
	}
	if entry.APIKey != "capy_legacy_key" {
		t.Fatalf("api key = %q, want capy_legacy_key", entry.APIKey)
	}
	if entry.Name != "Legacy Org" || entry.Slug != "legacy-org" {
		t.Fatalf("unexpected org metadata: %#v", entry)
	}

	// The migration must persist the new shape exactly once.
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	if !strings.Contains(string(migrated), `"organizations"`) {
		t.Fatalf("migrated config not written back in the multi-org shape:\n%s", string(migrated))
	}
	var onDisk UserConfig
	if err := json.Unmarshal(migrated, &onDisk); err != nil {
		t.Fatalf("decode migrated config: %v", err)
	}
	if onDisk.ActiveOrg != "org_legacy" {
		t.Fatalf("on-disk active_org = %q, want org_legacy", onDisk.ActiveOrg)
	}
	if _, found := onDisk.Organizations["org_legacy"]; !found {
		t.Fatalf("on-disk config missing migrated organization: %#v", onDisk)
	}
}

func TestLoadUserConfigMigratesLegacyShapeWithoutOrgIDUnderDefaultKey(t *testing.T) {
	isolateUserConfig(t)

	writeUserConfigFile(t, `{"api_key": "capy_only_key", "api_url": "https://capydb.dev/api/capydb"}`)

	cfg, err := LoadUserConfig()
	if err != nil {
		t.Fatalf("load user config: %v", err)
	}
	orgID, entry, ok := cfg.Active()
	if !ok {
		t.Fatalf("expected an active entry: %#v", cfg)
	}
	if orgID != DefaultOrgKey {
		t.Fatalf("active org = %q, want %q", orgID, DefaultOrgKey)
	}
	if entry.APIKey != "capy_only_key" {
		t.Fatalf("api key = %q, want capy_only_key", entry.APIKey)
	}
}

func TestLoadUserConfigMissingFileIsEmpty(t *testing.T) {
	isolateUserConfig(t)

	cfg, err := LoadUserConfig()
	if err != nil {
		t.Fatalf("load user config: %v", err)
	}
	if _, _, ok := cfg.Active(); ok {
		t.Fatalf("expected no active organization for a missing config: %#v", cfg)
	}
}

func TestUpsertAddsOrganizationsAndSwitchesActive(t *testing.T) {
	t.Setenv("CAPYDB_API_URL", "")
	t.Setenv("CAPYDB_APP_URL", "")

	var cfg UserConfig
	cfg.Upsert("org_a", OrganizationConfig{APIKey: "key_a", Name: "Org A"})
	cfg.Upsert("org_b", OrganizationConfig{APIKey: "key_b", Name: "Org B"})

	if cfg.ActiveOrg != "org_b" {
		t.Fatalf("active org = %q, want org_b", cfg.ActiveOrg)
	}
	if len(cfg.Organizations) != 2 {
		t.Fatalf("organizations = %d, want 2", len(cfg.Organizations))
	}
	if cfg.APIKey() != "key_b" {
		t.Fatalf("APIKey() = %q, want key_b", cfg.APIKey())
	}
	if cfg.Organizations["org_a"].APIKey != "key_a" {
		t.Fatalf("org_a entry lost: %#v", cfg.Organizations)
	}
	if got := cfg.Organizations["org_b"].APIURL; got != "https://capydb.dev/api/capydb" {
		t.Fatalf("default api url not applied: %q", got)
	}
}

func TestActiveSelfHealsDanglingActiveOrgWithSingleEntry(t *testing.T) {
	cfg := UserConfig{
		ActiveOrg: "org_gone",
		Organizations: map[string]OrganizationConfig{
			"org_only": {APIKey: "key_only", APIURL: "https://capydb.dev/api/capydb"},
		},
	}

	orgID, entry, ok := cfg.Active()
	if !ok {
		t.Fatalf("expected the single entry to become active")
	}
	if orgID != "org_only" || entry.APIKey != "key_only" {
		t.Fatalf("unexpected active entry: %s %#v", orgID, entry)
	}
}
