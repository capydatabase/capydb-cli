package scan

import (
	"slices"
	"testing"
)

func TestClassifyHost(t *testing.T) {
	cases := map[string]string{
		"ep-cool-rain-123.eu-central-1.aws.neon.tech":              ProviderNeon,
		"ep-cool-rain-123-pooler.aws.neon.tech":                    ProviderNeon,
		"db.abcdefghijklm.supabase.co":                             ProviderSupabase,
		"aws-0-eu-west-2.pooler.supabase.com":                      ProviderSupabase,
		"my-server.postgres.database.azure.com":                    ProviderAzure,
		"shop.abcdef.eu-west-1.rds.amazonaws.com":                  ProviderRDS,
		"shop.cluster-abcdef.eu-west-1.rds.amazonaws.com":          ProviderAurora,
		"shop.cluster-ro-abcdef.us-east-1.rds.amazonaws.com":       ProviderAurora,
		"ec2-34-254-86-177.eu-west-1.compute.amazonaws.com":        ProviderHeroku,
		"ec2-52-1-2-3.compute-1.amazonaws.com":                     ProviderHeroku,
		"abcdefghij-useast1-1.horizon.psdb.cloud":                  ProviderPlanetScale,
		"abc.tsdb.cloud.timescale.com":                             ProviderTimescale,
		"p.abc123.db.postgresbridge.com":                           ProviderCrunchy,
		"db-postgresql-lon1-123-do-user-1-0.db.ondigitalocean.com": ProviderDigitalO,
		"dpg-abc123-a.oregon-postgres.render.com":                  ProviderRender,
		"dpg-abc123-a.frankfurt-postgres.render.com":               ProviderRender,
		// A non-database render.com host must not be claimed as Postgres.
		"my-web-service.onrender.com":          ProviderOther,
		"roundhouse.proxy.rlwy.net":            ProviderRailway,
		"pg-123abc-myproject.a.aivencloud.com": ProviderAiven,
		"tenant-abc.db.capydb.dev":             ProviderCapyDB,
		"localhost":                            ProviderLocal,
		"postgres":                             ProviderLocal,
		"db.internal.example.com":              ProviderOther,
	}
	for host, want := range cases {
		if got := classifyHost(host); got != want {
			t.Errorf("classifyHost(%q) = %q, want %q", host, got, want)
		}
	}
}

// Aurora endpoints carry every RDS marker too. Classifying one as plain RDS
// would hand the customer the instance-parameter-group remediation for a
// cluster, which does nothing.
func TestClassifyHostAuroraBeatsRDS(t *testing.T) {
	const aurora = "shop.cluster-abcdef.eu-west-1.rds.amazonaws.com"
	if got := classifyHost(aurora); got != ProviderAurora {
		t.Fatalf("aurora endpoint classified as %q", got)
	}
	if ProfileFor(ProviderAurora).LogicalFix == ProfileFor(ProviderRDS).LogicalFix {
		t.Fatal("aurora and rds must not share a remediation: one is a cluster parameter group, the other an instance one")
	}
}

func TestIsPooledURL(t *testing.T) {
	cases := map[string]bool{
		"postgres://u:p@ep-cool-123-pooler.aws.neon.tech:5432/db":   true,
		"postgres://u:p@ep-cool-123.aws.neon.tech:5432/db":          false,
		"postgres://u:p@aws-0-eu-west-2.pooler.supabase.com:6543/d": true,
		// Supabase's session pooler on 5432 holds session state, so it can
		// serve a consistent dump.
		"postgres://u:p@aws-0-eu-west-2.pooler.supabase.com:5432/d": false,
		// PlanetScale's PSBouncer and CapyDB's own pooler both sit on 6432.
		"postgres://u:p@abc-useast1-1.horizon.psdb.cloud:6432/db": true,
		"postgres://u:p@abc-useast1-1.horizon.psdb.cloud:5432/db": false,
		"":            false,
		"::not a url": false,
	}
	for raw, want := range cases {
		if got := IsPooledURL(raw); got != want {
			t.Errorf("IsPooledURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// Every catalogued provider must carry either a remediation or an explicit
// statement that there is none. A profile with neither is a provider we added
// without learning anything about it, which is worse than not listing it.
func TestProfilesCarryAPrecondition(t *testing.T) {
	for _, id := range KnownProviders() {
		profile := ProfileFor(id)
		if profile.Name == "" {
			t.Errorf("%s: no display name", id)
		}
		switch profile.Logical {
		case LogicalNever:
			if profile.LogicalFix != "" {
				t.Errorf("%s: claims logical replication is unavailable but offers a fix", id)
			}
			if profile.FollowPath != PathDumpRestore {
				t.Errorf("%s: cannot stream, so the recommended path must be dump-restore", id)
			}
		case LogicalOptIn:
			if profile.LogicalFix == "" {
				t.Errorf("%s: opt-in logical replication needs the remediation spelled out", id)
			}
		case LogicalOn:
			// Nothing to do: on by default needs no remediation.
		default:
			t.Errorf("%s: unknown logical-replication posture %q", id, profile.Logical)
		}
	}
}

func TestProfileForUnknownProvider(t *testing.T) {
	profile := ProfileFor("something-new")
	if profile.Name != "Self-managed PostgreSQL" || profile.LogicalFix == "" {
		t.Fatalf("unknown provider must fall back to a usable generic profile, got %+v", profile)
	}
	if slices.Contains(KnownProviders(), "something-new") {
		t.Fatal("ProfileFor must not register unknown providers")
	}
}
