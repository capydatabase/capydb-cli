package scan

import (
	"sort"
	"strings"
)

// Source-provider catalog. Two things live here and nothing else:
//
//  1. Identification - a hostname rule for the repo-only scan, and (in
//     source.go) a live-session rule for the case the hostname cannot answer.
//     Hostnames are the weaker signal by construction: Cloud SQL and AlloyDB
//     have no public DNS name at all (an IP, or the /cloudsql unix socket),
//     Heroku Postgres hands out ordinary EC2 names, and anything behind a
//     bastion arrives as an address. When both signals exist the live one wins.
//
//  2. The one fact a migration plan turns on that the source cannot report
//     itself: WHY logical replication is off and what the customer has to do
//     about it. The probe measures `wal_level`; the catalog only supplies the
//     remediation, so a provider whose defaults change does not make us wrong.
//
// Every entry here must carry a verified precondition. A provider with nothing
// specific to say is noise - it classifies as `other` and the generic advice
// applies.

// Provider identifiers. The zero value is deliberately absent: an unclassified
// host is ProviderOther, never "".
const (
	ProviderNeon        = "neon"
	ProviderSupabase    = "supabase"
	ProviderAzure       = "azure"
	ProviderRDS         = "rds"
	ProviderAurora      = "aurora"
	ProviderCloudSQL    = "cloudsql"
	ProviderAlloyDB     = "alloydb"
	ProviderHeroku      = "heroku"
	ProviderPlanetScale = "planetscale"
	ProviderTimescale   = "timescale"
	ProviderCrunchy     = "crunchy"
	ProviderDigitalO    = "digitalocean"
	ProviderRender      = "render"
	ProviderRailway     = "railway"
	ProviderAiven       = "aiven"
	ProviderCapyDB      = "capydb"
	ProviderLocal       = "local"
	ProviderOther       = "other"
)

// Logical-replication postures. These describe the PROVIDER's default, not the
// database in front of us - the probe reports that.
const (
	// LogicalOn: logical decoding is on out of the box.
	LogicalOn = "on-by-default"
	// LogicalOptIn: supported, but off until the customer flips something and
	// (usually) restarts.
	LogicalOptIn = "opt-in"
	// LogicalNever: the provider does not offer it at all, so `capydb import
	// --follow` is impossible and the migration is a dump/restore with a
	// maintenance window.
	LogicalNever = "unavailable"
)

// Profile is what CapyDB knows about one source provider.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Logical is the provider's default logical-replication posture, one of
	// the Logical* constants above.
	Logical string `json:"logical_replication"`
	// LogicalFix is the exact remediation when the probe finds logical
	// decoding off. Empty for LogicalNever - there is no fix.
	LogicalFix string `json:"logical_replication_fix,omitempty"`
	// FollowPath is the migration shape CapyDB recommends for this source.
	FollowPath string `json:"recommended_path"`
	// Notes are provider-specific facts a migration plan needs. Customer-facing
	// copy: plain Postgres vocabulary, no CapyDB infrastructure detail.
	Notes []string `json:"notes,omitempty"`
}

// profiles is keyed by the identifiers above. ProviderLocal and ProviderOther
// are deliberately absent: they carry no provider-specific precondition, and
// ProfileFor synthesizes a generic entry for them.
var profiles = map[string]Profile{
	ProviderSupabase: {
		ID: ProviderSupabase, Name: "Supabase",
		Logical:    LogicalOn,
		FollowPath: "follow",
		Notes: []string{
			"Dump from the direct endpoint or the session pooler on port 5432 - the transaction pooler on port 6543 cannot serve a consistent dump.",
			"The auth, storage, realtime and vault schemas are Supabase's own and do not move; anything referencing them needs an answer before cutover.",
			"RLS policies calling auth.uid() are inert on plain Postgres - convert them with `capydb migrate rls` or move authorization into the app.",
		},
	},
	ProviderNeon: {
		ID: ProviderNeon, Name: "Neon",
		Logical:    LogicalOptIn,
		LogicalFix: "Enable logical replication for the project in the Neon console (Settings -> Logical Replication), then reconnect.",
		FollowPath: "follow",
		Notes: []string{
			"Dump from the direct endpoint: a '-pooler' hostname cannot serve a consistent dump.",
			"Client code on @neondatabase/serverless moves to a standard driver - `capydb migrate codemod neon` rewrites it.",
		},
	},
	ProviderPlanetScale: {
		ID: ProviderPlanetScale, Name: "PlanetScale for Postgres",
		Logical:    LogicalOptIn,
		LogicalFix: "Confirm logical replication is enabled for the branch, and connect on the direct port (5432) rather than through PSBouncer.",
		FollowPath: "follow",
		Notes: []string{
			"Connect on the direct port - PSBouncer on port 6432 pools transactions and cannot serve a consistent dump.",
			"Only Postgres sources are covered here; a Vitess or MySQL database is a different migration and not one CapyDB performs.",
		},
	},
	ProviderRDS: {
		ID: ProviderRDS, Name: "Amazon RDS for PostgreSQL",
		Logical:    LogicalOptIn,
		LogicalFix: "Set rds.logical_replication=1 in the instance's parameter group and reboot the instance, then grant rds_replication to the migration role.",
		FollowPath: "follow",
		Notes: []string{
			"The master user is not a superuser: event triggers and extensions outside the RDS catalog do not move.",
		},
	},
	ProviderAurora: {
		ID: ProviderAurora, Name: "Amazon Aurora PostgreSQL",
		Logical:    LogicalOptIn,
		LogicalFix: "Set rds.logical_replication=1 in the cluster parameter group and reboot the writer, then grant rds_replication to the migration role.",
		FollowPath: "follow",
		Notes: []string{
			"Point the migration at the writer endpoint: a reader endpoint cannot create a replication slot.",
		},
	},
	ProviderCloudSQL: {
		ID: ProviderCloudSQL, Name: "Google Cloud SQL for PostgreSQL",
		Logical:    LogicalOptIn,
		LogicalFix: "Set the cloudsql.logical_decoding flag to on and restart the instance, then grant the cloudsqlsuperuser-owned migration role REPLICATION.",
		FollowPath: "follow",
		Notes: []string{
			"Cloud SQL has no public hostname by default - the migration needs an authorized network or a proxy that CapyDB can reach.",
		},
	},
	ProviderAlloyDB: {
		ID: ProviderAlloyDB, Name: "Google AlloyDB for PostgreSQL",
		Logical:    LogicalOptIn,
		LogicalFix: "Set the alloydb.logical_decoding flag to on and restart the primary instance - the flag name differs from Cloud SQL's.",
		FollowPath: "follow",
		Notes: []string{
			"AlloyDB's columnar engine and its google_* extensions are AlloyDB-only and have no equivalent on plain Postgres.",
			"AlloyDB is reachable on a private address only - the migration needs a route CapyDB can reach.",
		},
	},
	ProviderAzure: {
		ID: ProviderAzure, Name: "Azure Database for PostgreSQL",
		Logical:    LogicalOptIn,
		LogicalFix: "Set the wal_level server parameter to logical and restart the server, then ALTER ROLE <migration role> WITH REPLICATION and grant it azure_pg_admin.",
		FollowPath: "follow",
		Notes: []string{
			"Single Server is retired; these steps describe Flexible Server.",
		},
	},
	ProviderHeroku: {
		ID: ProviderHeroku, Name: "Heroku Postgres",
		Logical:    LogicalNever,
		FollowPath: "dump-restore",
		Notes: []string{
			"Heroku Postgres offers neither logical replication nor superuser, so a streaming import is not possible - the migration is a dump and restore inside a maintenance window.",
			"Take the dump from the DATABASE_URL credentials directly; Heroku rotates them, so re-read the config var rather than reusing a stored copy.",
		},
	},
	ProviderTimescale: {
		ID: ProviderTimescale, Name: "Timescale Cloud",
		Logical:    LogicalOptIn,
		LogicalFix: "Enable logical replication for the service, then restart it.",
		FollowPath: "dump-restore",
		Notes: []string{
			"Hypertables are a timescaledb construct. CapyDB does not offer timescaledb, so hypertables have to become ordinary or natively partitioned tables before the data moves.",
		},
	},
	ProviderCrunchy: {
		ID: ProviderCrunchy, Name: "Crunchy Bridge",
		Logical:    LogicalOn,
		FollowPath: "follow",
	},
	ProviderDigitalO: {
		ID: ProviderDigitalO, Name: "DigitalOcean Managed Postgres",
		Logical:    LogicalOn,
		FollowPath: "follow",
		Notes: []string{
			"Dump from the primary connection details, not the connection pool.",
		},
	},
	ProviderRender: {
		ID: ProviderRender, Name: "Render Postgres",
		Logical:    LogicalOptIn,
		LogicalFix: "Enable logical replication in the database's settings, then restart it.",
		FollowPath: "follow",
		Notes: []string{
			"Use the external connection string - the internal one resolves only inside Render.",
		},
	},
	ProviderRailway: {
		ID: ProviderRailway, Name: "Railway Postgres",
		Logical:    LogicalOptIn,
		LogicalFix: "Set wal_level=logical on the Postgres service and redeploy it.",
		FollowPath: "follow",
		Notes: []string{
			"Use the public proxy connection string - the internal hostname resolves only inside the Railway project.",
		},
	},
	ProviderAiven: {
		ID: ProviderAiven, Name: "Aiven for PostgreSQL",
		Logical:    LogicalOn,
		FollowPath: "follow",
	},
	ProviderCapyDB: {
		ID: ProviderCapyDB, Name: "CapyDB",
		Logical:    LogicalOn,
		FollowPath: "follow",
		Notes: []string{
			"This is already a CapyDB cell - to copy it, branch it or restore it rather than importing it.",
		},
	},
}

// ProfileFor returns the catalog entry for a provider identifier, synthesizing
// a generic profile for a self-managed or unrecognized source. Never nil-ish:
// callers render the result directly.
func ProfileFor(provider string) Profile {
	if profile, ok := profiles[provider]; ok {
		return profile
	}
	name := "Self-managed PostgreSQL"
	if provider == ProviderLocal {
		name = "Local PostgreSQL"
	}
	return Profile{
		ID:      provider,
		Name:    name,
		Logical: LogicalOptIn,
		LogicalFix: "Set wal_level=logical, max_replication_slots and max_wal_senders to at least 4 in postgresql.conf and restart, " +
			"then give the migration role the REPLICATION attribute.",
		FollowPath: "follow",
	}
}

// KnownProviders lists every catalogued identifier, sorted. Used by the docs
// generator and the scan test that asserts catalog/detector agreement.
func KnownProviders() []string {
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// classifyHost maps a database hostname to a provider identifier. It is the
// only classifier available to a repo-only scan; ClassifyServer (source.go)
// overrides it whenever a live session disagrees.
func classifyHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case strings.HasSuffix(host, ".neon.tech"):
		return ProviderNeon
	case strings.HasSuffix(host, ".supabase.com") || strings.HasSuffix(host, ".supabase.co"):
		return ProviderSupabase
	case strings.HasSuffix(host, ".postgres.database.azure.com"):
		return ProviderAzure
	// Heroku Postgres runs on plain EC2 names (ec2-<ip>.<region>.compute.amazonaws.com,
	// and compute-1 for the original us-east-1 estate). Checked before the RDS
	// rules because both live under amazonaws.com.
	case strings.HasPrefix(host, "ec2-") && strings.Contains(host, ".compute"):
		return ProviderHeroku
	// Aurora writer/reader endpoints carry a .cluster-/.cluster-ro- segment
	// that plain RDS instances never have; the posture differs (cluster
	// parameter group, writer-only slots), so they are separate entries.
	case strings.Contains(host, ".cluster-") && strings.Contains(host, ".rds.amazonaws.com"):
		return ProviderAurora
	case strings.Contains(host, ".rds.amazonaws.com"):
		return ProviderRDS
	case strings.HasSuffix(host, ".psdb.cloud"):
		return ProviderPlanetScale
	case strings.HasSuffix(host, ".tsdb.cloud.timescale.com"):
		return ProviderTimescale
	case strings.HasSuffix(host, ".db.postgresbridge.com"):
		return ProviderCrunchy
	case strings.HasSuffix(host, ".db.ondigitalocean.com"):
		return ProviderDigitalO
	// Render's external hostnames are region-scoped and the exact suffix is not
	// documented verbatim, so this matches the two invariants that are: the
	// `dpg-` instance prefix and the render.com domain. A host this misses
	// falls through to the generic advice rather than to a wrong provider -
	// there is no other product on render.com that would take a Postgres URL.
	case strings.HasSuffix(host, ".render.com") &&
		(strings.HasPrefix(host, "dpg-") || strings.Contains(host, "-postgres.render.com")):
		return ProviderRender
	case strings.HasSuffix(host, ".railway.app") || strings.HasSuffix(host, ".rlwy.net"):
		return ProviderRailway
	case strings.HasSuffix(host, ".aivencloud.com"):
		return ProviderAiven
	case strings.HasSuffix(host, ".db.capydb.dev") || host == "db1.capydb.dev":
		return ProviderCapyDB
	case host == "localhost" || host == "127.0.0.1" || !strings.Contains(host, "."):
		return ProviderLocal
	default:
		return ProviderOther
	}
}
