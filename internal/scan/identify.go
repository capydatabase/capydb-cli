package scan

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Live identification of the source: who runs it, and whether it can stream.
//
// Both answers have to come from the server. Hostnames cannot identify Cloud
// SQL or AlloyDB (no public DNS name exists), they call Heroku Postgres an EC2
// instance, and they say nothing at all about a database reached through a
// bastion. And the provider's documented default is not evidence about the
// database in front of us: `wal_level` is a setting, someone may have already
// changed it, and a migration plan that assumes either way is wrong half the
// time. So: measure, then use the catalog only to explain what to do about the
// measurement.

// providerSignalQuery collects every catalog fact that identifies a managed
// provider, in one round trip. Each row is "<kind>:<name>"; classifyServer
// turns the set into a provider identifier.
//
// pg_settings and pg_roles are world-readable, and every predicate is an
// equality or a prefix over a small catalog, so this stays cheap on a locked
// down managed source where most other probes would be refused.
const providerSignalQuery = `
	select 'role:' || rolname from pg_catalog.pg_roles
	where rolname in ('rds_superuser', 'rdsadmin', 'cloudsqladmin', 'cloudsqlsuperuser',
	                  'alloydbadmin', 'alloydbsuperuser', 'azure_pg_admin', 'azuresu',
	                  'supabase_admin', 'neondb_owner')
	union all
	select 'schema:' || nspname from pg_catalog.pg_namespace
	where nspname in ('heroku_ext', 'auth', 'storage', 'realtime', 'graphql', 'capydb')
	union all
	select 'ext:' || extname from pg_catalog.pg_extension
	where extname in ('neon', 'neon_utils', 'supabase_vault', 'pg_graphql', 'timescaledb',
	                  'google_columnar_engine', 'google_ml_integration', 'aiven_extras')
	union all
	select distinct 'guc:' || split_part(name, '.', 1) from pg_catalog.pg_settings
	where name like 'rds.%' or name like 'cloudsql.%' or name like 'alloydb.%'
	   or name like 'azure.%' or name like 'neon.%'
	union all
	select 'proc:aurora_version' from pg_catalog.pg_proc where proname = 'aurora_version'`

// serverRules maps provider identifiers to the signals that prove them, in
// evaluation order. Order is load-bearing:
//
//   - Aurora before RDS: an Aurora cluster carries every RDS signal too, and
//     only Aurora's cluster parameter group and writer-only replication slots
//     make its remediation correct.
//   - AlloyDB before Cloud SQL: same relationship, and the logical-decoding
//     flag is spelled differently on each.
//   - Heroku first: it is the one provider where a streaming import is
//     impossible, so a mistaken RDS classification would recommend a migration
//     path that cannot run.
//
// timescaledb is deliberately NOT a classification signal. The extension
// installs on any Postgres, so its presence identifies a schema shape rather
// than a host; it already surfaces as an unavailable extension, which produces
// the correct blocker either way.
var serverRules = []struct {
	provider string
	signals  []string
}{
	{ProviderHeroku, []string{"schema:heroku_ext"}},
	{ProviderAurora, []string{"proc:aurora_version"}},
	{ProviderRDS, []string{"role:rds_superuser", "role:rdsadmin", "guc:rds"}},
	{ProviderAlloyDB, []string{"guc:alloydb", "role:alloydbadmin", "role:alloydbsuperuser",
		"ext:google_columnar_engine", "ext:google_ml_integration"}},
	{ProviderCloudSQL, []string{"role:cloudsqladmin", "role:cloudsqlsuperuser", "guc:cloudsql"}},
	{ProviderAzure, []string{"role:azure_pg_admin", "role:azuresu", "guc:azure"}},
	{ProviderNeon, []string{"ext:neon", "ext:neon_utils", "role:neondb_owner", "guc:neon"}},
	{ProviderSupabase, []string{"role:supabase_admin", "ext:supabase_vault"}},
	{ProviderAiven, []string{"ext:aiven_extras"}},
	{ProviderCapyDB, []string{"schema:capydb"}},
}

// classifyServer maps the collected signals to a provider identifier and
// returns the signals that decided it. Pure, so the ordering above is testable
// without a database. Returns ProviderOther when nothing matches - a
// self-managed source is a legitimate answer, not a failure.
func classifyServer(signals []string) (string, []string) {
	present := make(map[string]bool, len(signals))
	for _, signal := range signals {
		present[signal] = true
	}

	for _, rule := range serverRules {
		var matched []string
		for _, signal := range rule.signals {
			if present[signal] {
				matched = append(matched, signal)
			}
		}
		if len(matched) > 0 {
			return rule.provider, matched
		}
	}

	// Supabase's dedicated role is invisible to a tenant role on some plans;
	// its three managed schemas together are still conclusive (any one alone is
	// not - plenty of applications own a schema called `auth`).
	if present["schema:auth"] && present["schema:storage"] && present["schema:realtime"] {
		return ProviderSupabase, []string{"schema:auth", "schema:storage", "schema:realtime"}
	}
	return ProviderOther, nil
}

func probeProvider(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	rows, err := conn.QueryContext(ctx, providerSignalQuery)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var signals []string
	for rows.Next() {
		var signal string
		if err := rows.Scan(&signal); err != nil {
			return err
		}
		signals = append(signals, signal)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	provider, matched := classifyServer(signals)
	facts.Provider = provider
	sort.Strings(matched)
	facts.ProviderSignals = matched
	return nil
}

// SourceReplication is the source's measured readiness for a streaming import.
// Everything here is a fact read out of the running server; the provider
// catalog only supplies the remediation for whatever is missing.
type SourceReplication struct {
	WALLevel            string `json:"wal_level"`
	MaxReplicationSlots int    `json:"max_replication_slots"`
	MaxWALSenders       int    `json:"max_wal_senders"`
	UsedSlots           int    `json:"used_replication_slots"`
	// RoleCanReplicate reports whether the role we connected as carries the
	// REPLICATION attribute (or is a superuser). Without it the publisher
	// refuses to open a slot, and the failure lands at import time.
	RoleCanReplicate bool `json:"role_can_replicate"`
	// Ready is the whole judgement: this source can serve `capydb import
	// --follow` right now, as connected.
	Ready bool `json:"ready"`
	// Blockers explain a false Ready, in the order they must be fixed.
	Blockers []string `json:"blockers"`
}

func probeReplication(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	replication := &facts.Replication
	if err := conn.QueryRowContext(ctx, `
		select current_setting('wal_level'),
		       current_setting('max_replication_slots')::int,
		       current_setting('max_wal_senders')::int,
		       (select count(*) from pg_catalog.pg_replication_slots),
		       (select bool_or(rolreplication or rolsuper)
		        from pg_catalog.pg_roles where rolname = current_user)`).
		Scan(&replication.WALLevel, &replication.MaxReplicationSlots, &replication.MaxWALSenders,
			&replication.UsedSlots, &replication.RoleCanReplicate); err != nil {
		return err
	}
	replication.Blockers = replicationBlockers(*replication, facts.Inventory)
	replication.Ready = len(replication.Blockers) == 0
	return nil
}

// replicationBlockers lists, in fix order, everything standing between this
// source and a streaming import. Separated from the probe so the same rules
// grade a source the CLI read and one the control plane inspected.
//
// Note this runs before probeInventory on a live scan, so the primary-key
// blocker is added afterwards by refreshReplicationReadiness. Ordering the
// probes the other way round would be worse: replication settings are the
// cheap probe and the one most likely to be refused outright.
func replicationBlockers(replication SourceReplication, inventory SourceInventory) []string {
	var blockers []string
	if !strings.EqualFold(replication.WALLevel, "logical") {
		blockers = append(blockers, fmt.Sprintf(
			"wal_level is %q, and a streaming import needs \"logical\"", replication.WALLevel))
	}
	if replication.MaxReplicationSlots-replication.UsedSlots < 1 {
		blockers = append(blockers, fmt.Sprintf(
			"no free replication slot: %d of %d are in use",
			replication.UsedSlots, replication.MaxReplicationSlots))
	}
	if replication.MaxWALSenders < 1 {
		blockers = append(blockers, "max_wal_senders is 0, so no client can stream changes")
	}
	if !replication.RoleCanReplicate {
		blockers = append(blockers, "the role you connected as does not have the REPLICATION attribute")
	}
	if count := len(inventory.TablesWithoutReplicaIdentity); count > 0 {
		blockers = append(blockers, fmt.Sprintf(
			"%d table(s) have neither a primary key nor a replica identity, so their updates cannot be streamed: %s",
			count, strings.Join(firstNames(inventory.TablesWithoutReplicaIdentity, 5), ", ")))
	}
	return blockers
}

// NeedsServerConfig reports whether the source is blocked by something only a
// server setting or a role grant can fix - as opposed to a schema problem the
// provider's documentation says nothing about. The distinction decides whether
// a report should quote the provider's remediation at all: telling somebody
// whose wal_level is already logical to set wal_level=logical reads as a tool
// that did not look.
func (r SourceReplication) NeedsServerConfig() bool {
	return !strings.EqualFold(r.WALLevel, "logical") ||
		r.MaxReplicationSlots-r.UsedSlots < 1 ||
		r.MaxWALSenders < 1 ||
		!r.RoleCanReplicate
}

// refreshReplicationReadiness re-grades replication once the inventory is in
// hand, so the primary-key finding participates in the verdict.
func (f *SourceFacts) refreshReplicationReadiness() {
	f.Replication.Blockers = replicationBlockers(f.Replication, f.Inventory)
	f.Replication.Ready = len(f.Replication.Blockers) == 0
}

// Profile returns the catalog entry for the provider the server identified,
// falling back to the generic self-managed profile.
func (f *SourceFacts) Profile() Profile { return ProfileFor(f.Provider) }

// firstNames caps a list for display, appending an ellipsis entry when it had
// to cut. Kept separate from firstN in scan.go: that one is unexported there
// and this file must not depend on its callers' formatting.
func firstNames(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	capped := slices.Clone(values[:limit])
	return append(capped, fmt.Sprintf("and %d more", len(values)-limit))
}
