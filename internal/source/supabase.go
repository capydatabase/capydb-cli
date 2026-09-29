package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SupabaseAPIURL is the Supabase Management API base URL.
const SupabaseAPIURL = "https://api.supabase.com"

// SupabaseSource describes a Supabase connection string.
type SupabaseSource struct {
	// Ref is the Supabase project ref.
	Ref string
	// Direct is true for db.<ref>.supabase.co, the direct host. On current
	// Supabase projects it resolves to IPv6 only.
	Direct bool
	// PoolerHost is the shared pooler host from the string (aws-N-<region>.
	// pooler.supabase.com), empty for a direct host.
	PoolerHost string
	Port       string
}

// DetectSupabase recognises a Supabase source and its project ref: from the
// direct host (db.<ref>.supabase.co), or from the pooler username
// (postgres.<ref>) on the shared pooler host, which does not carry the ref.
func DetectSupabase(endpoint Endpoint) (SupabaseSource, bool) {
	for _, host := range endpoint.Hosts {
		lower := strings.ToLower(strings.TrimSuffix(host, "."))
		if ref, ok := strings.CutPrefix(lower, "db."); ok {
			if ref, ok = strings.CutSuffix(ref, ".supabase.co"); ok && ref != "" && !strings.Contains(ref, ".") {
				return SupabaseSource{Ref: ref, Direct: true, Port: endpoint.Port}, true
			}
		}
		if strings.HasSuffix(lower, ".pooler.supabase.com") {
			_, ref, _ := strings.Cut(endpoint.User, ".")
			return SupabaseSource{Ref: ref, PoolerHost: lower, Port: endpoint.Port}, true
		}
	}
	return SupabaseSource{}, false
}

// SupabasePooler is one entry of GET /v1/projects/{ref}/config/database/pooler.
type SupabasePooler struct {
	DatabaseType string `json:"database_type"`
	DBHost       string `json:"db_host"`
	DBName       string `json:"db_name"`
	DBPort       int    `json:"db_port"`
	DBUser       string `json:"db_user"`
	PoolMode     string `json:"pool_mode"`
}

// FetchSupabasePrimaryPooler reads the project's pooler configuration from
// the Supabase Management API and returns the PRIMARY database's entry. The
// aws-0 / aws-1 host prefix differs per project, and this is the only
// authoritative source for it.
func FetchSupabasePrimaryPooler(ctx context.Context, client *http.Client, baseURL, token, ref string) (SupabasePooler, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/projects/" + url.PathEscape(ref) + "/config/database/pooler"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SupabasePooler{}, fmt.Errorf("build pooler config request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return SupabasePooler{}, fmt.Errorf("call the Supabase Management API: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return SupabasePooler{}, fmt.Errorf("read the Supabase Management API response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return SupabasePooler{}, fmt.Errorf("the Supabase Management API returned %s for project %s", response.Status, ref)
	}
	var poolers []SupabasePooler
	if err := json.Unmarshal(body, &poolers); err != nil {
		return SupabasePooler{}, fmt.Errorf("decode the Supabase pooler config: %w", err)
	}
	for _, pooler := range poolers {
		if strings.EqualFold(pooler.DatabaseType, "PRIMARY") && pooler.DBHost != "" {
			return pooler, nil
		}
	}
	return SupabasePooler{}, fmt.Errorf("the Supabase Management API listed no pooler for the primary database of project %s", ref)
}

// SupabaseAdvice returns the lines to show for a Supabase source, given the
// pooler the Management API reported (nil when it could not be read). The
// session pooler on port 5432 is the path to recommend: the direct host is
// IPv6-only on current projects, and the transaction pooler on 6543 breaks
// pg_dump.
func SupabaseAdvice(src SupabaseSource, pooler *SupabasePooler) []string {
	if pooler == nil {
		if src.Ref == "" {
			return []string{"Supabase pooler username should be postgres.<project-ref>; copy the Session pooler string from the Supabase dashboard (Connect -> Session pooler)."}
		}
		lines := []string{}
		if src.Direct {
			lines = append(lines, "db."+src.Ref+".supabase.co is IPv6-only on current Supabase projects; if it cannot be reached, use the session pooler (port 5432).")
		}
		return append(lines,
			"The Supabase pooler host prefix (aws-0 / aws-1) differs per project. Copy the Session pooler string from the Supabase dashboard (Connect -> Session pooler), or set SUPABASE_ACCESS_TOKEN and the CLI reads it for you:",
			"  curl -s -H \"Authorization: Bearer $SUPABASE_ACCESS_TOKEN\" "+SupabaseAPIURL+"/v1/projects/"+src.Ref+"/config/database/pooler",
		)
	}

	session := fmt.Sprintf("%s:5432 (user %s)", pooler.DBHost, firstNonEmpty(pooler.DBUser, "postgres."+src.Ref))
	switch {
	case src.Direct:
		return []string{"db." + src.Ref + ".supabase.co is IPv6-only on current Supabase projects; if it cannot be reached, use this project's session pooler: " + session + "."}
	case !strings.EqualFold(src.PoolerHost, pooler.DBHost):
		return []string{fmt.Sprintf("Supabase reports this project's pooler at %s, not %s: use the session pooler %s.", pooler.DBHost, src.PoolerHost, session)}
	case src.Port == "6543":
		return []string{"Port 6543 is Supabase's transaction pooler, which breaks pg_dump; use the session pooler " + session + "."}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
