package cli

import (
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
