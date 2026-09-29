package cli

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Provider presets for `capydb import --from <provider>`: each rewrites the
// source URL into the form an import needs (the direct or session endpoint,
// TLS on) and states every change, and warns about the provider traps the
// dogfood migrations hit. A preset never guesses a host it cannot derive
// from the string itself - where the right endpoint lives only in the
// provider's dashboard, it stops and says where to find it.

type importPreset struct {
	name string
	// hostSuffixes identify the provider's hosts; a URL on another host gets
	// a warning (the preset still applies - custom domains exist).
	hostSuffixes []string
	apply        func(u *url.URL, result *presetResult) error
}

// presetResult is a rewritten source URL plus what happened to it.
type presetResult struct {
	URL      string   `json:"-"`
	Changes  []string `json:"changes"`
	Warnings []string `json:"warnings"`
}

var importPresets = map[string]importPreset{
	"neon": {
		name:         "neon",
		hostSuffixes: []string{".neon.tech"},
		apply: func(u *url.URL, result *presetResult) error {
			// Neon's pooled and direct endpoints differ only by "-pooler" in
			// the first host label (ep-x-pooler.region... vs ep-x.region...).
			host, port := u.Hostname(), u.Port()
			label, rest, _ := strings.Cut(host, ".")
			if direct, ok := strings.CutSuffix(label, "-pooler"); ok {
				setHost(u, direct+"."+rest, port)
				result.Changes = append(result.Changes, fmt.Sprintf("pooled endpoint %s -> direct endpoint %s (pg_dump needs a session, not PgBouncer)", host, u.Hostname()))
			}
			ensureSSLMode(u, result)
			result.Warnings = append(result.Warnings,
				"a suspended Neon compute wakes on the first connection; the import waits for it",
				"`capydb import --follow` needs logical replication enabled in the Neon project settings",
				"after the data moves, `capydb migrate codemod neon` swaps @neondatabase/serverless for postgres-js")
			return nil
		},
	},
	"supabase": {
		name:         "supabase",
		hostSuffixes: []string{".supabase.co", ".supabase.com"},
		apply: func(u *url.URL, result *presetResult) error {
			host, port := u.Hostname(), u.Port()
			if strings.HasSuffix(host, ".pooler.supabase.com") && port == "6543" {
				// Same pooler host, session mode.
				setHost(u, host, "5432")
				result.Changes = append(result.Changes, "transaction pooler port 6543 -> session pooler port 5432 (same host)")
			}
			if strings.HasPrefix(host, "db.") && strings.HasSuffix(host, ".supabase.co") {
				result.Warnings = append(result.Warnings, "the direct host db.<ref>.supabase.co resolves to IPv6 only on current projects; if the import cannot reach it, use the Session pooler string from Connect in the Supabase dashboard (aws-N-<region>.pooler.supabase.com:5432, user postgres.<ref>)")
			}
			ensureSSLMode(u, result)
			result.Warnings = append(result.Warnings,
				"Supabase-managed schemas (auth, storage, realtime, ...) are not imported; tables with foreign keys to auth.users and RLS policies calling auth.uid() need `capydb migrate rls` - or restore a dump with `capydb import --from-supabase-dump`, which applies the converted policies in the right order")
			return nil
		},
	},
	"planetscale": {
		name:         "planetscale",
		hostSuffixes: []string{".psdb.cloud"},
		apply: func(u *url.URL, result *presetResult) error {
			if u.Port() == "6432" {
				setHost(u, u.Hostname(), "5432")
				result.Changes = append(result.Changes, "PgBouncer port 6432 -> direct port 5432 (pg_dump needs a session)")
			}
			ensureSSLMode(u, result)
			return nil
		},
	},
	"railway": {
		name:         "railway",
		hostSuffixes: []string{".rlwy.net", ".railway.app"},
		apply: func(u *url.URL, result *presetResult) error {
			if strings.HasSuffix(u.Hostname(), ".railway.internal") {
				return fmt.Errorf("%s is Railway's private network, which only other Railway services reach; use the service's DATABASE_PUBLIC_URL (a *.proxy.rlwy.net host) instead", u.Hostname())
			}
			return nil
		},
	},
	"render": {
		name:         "render",
		hostSuffixes: []string{".render.com"},
		apply: func(u *url.URL, result *presetResult) error {
			if !strings.Contains(u.Hostname(), ".") {
				return fmt.Errorf("%s is Render's internal hostname, reachable only from Render services; use the External Database URL from the database's Connect menu (<id>.<region>-postgres.render.com)", u.Hostname())
			}
			ensureSSLMode(u, result)
			return nil
		},
	},
	"rds": {
		name:         "rds",
		hostSuffixes: []string{".rds.amazonaws.com"},
		apply: func(u *url.URL, result *presetResult) error {
			if strings.Contains(u.Hostname(), ".proxy-") {
				result.Warnings = append(result.Warnings, "this is an RDS Proxy endpoint; pg_dump works through it, but `--follow` (logical replication) needs the instance or cluster endpoint")
			}
			ensureSSLMode(u, result)
			result.Warnings = append(result.Warnings,
				"the instance must be publicly accessible with a security group that admits CapyDB's import network",
				"`capydb import --follow` needs rds.logical_replication = 1 in the parameter group")
			return nil
		},
	},
}

func importPresetNames() []string {
	names := make([]string, 0, len(importPresets))
	for name := range importPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// applyImportPreset rewrites raw for provider.
func applyImportPreset(provider, raw string) (presetResult, error) {
	preset, ok := importPresets[strings.ToLower(strings.TrimSpace(provider))]
	if !ok {
		return presetResult{}, usageErrorf("unknown --from %q; available: %s", provider, strings.Join(importPresetNames(), ", "))
	}
	lower := strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(lower, "postgres://") && !strings.HasPrefix(lower, "postgresql://") {
		return presetResult{}, usageErrorf("--from %s needs --source-url as a postgres:// URL", preset.name)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return presetResult{}, usageErrorf("parse --source-url: %v", err)
	}
	if strings.Contains(u.Host, ",") {
		return presetResult{}, usageErrorf("--from %s does not support multi-host connection strings", preset.name)
	}

	result := presetResult{Changes: []string{}, Warnings: []string{}}
	matched := false
	for _, suffix := range preset.hostSuffixes {
		if strings.HasSuffix(strings.ToLower(u.Hostname()), suffix) {
			matched = true
		}
	}
	if err := preset.apply(u, &result); err != nil {
		return presetResult{}, usageErrorf("%v", err)
	}
	if !matched {
		result.Warnings = append([]string{fmt.Sprintf("%s does not look like a %s host; applied the preset anyway", u.Hostname(), preset.name)}, result.Warnings...)
	}
	result.URL = u.String()
	return result, nil
}

func setHost(u *url.URL, host, port string) {
	if port == "" {
		u.Host = host
		return
	}
	u.Host = net.JoinHostPort(host, port)
}

// ensureSSLMode turns TLS on when the string leaves it to libpq's default
// ("prefer", which silently falls back to plaintext).
func ensureSSLMode(u *url.URL, result *presetResult) {
	query := u.Query()
	if query.Get("sslmode") != "" {
		return
	}
	query.Set("sslmode", "require")
	u.RawQuery = query.Encode()
	result.Changes = append(result.Changes, "added sslmode=require")
}
