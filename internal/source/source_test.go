package source

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		raw   string
		hosts []string
		port  string
		user  string
	}{
		{"postgres://u:p@db.example.com:6543/app", []string{"db.example.com"}, "6543", "u"},
		{"postgresql://u@[::1]:5432/app", []string{"::1"}, "5432", "u"},
		{"postgres:///app?host=/var/run/postgresql", []string{"/var/run/postgresql"}, "", ""},
		{"postgres://u@h1:5432,h2:5433/app", []string{"h1", "h2"}, "5432", "u"},
		{"host=10.0.0.5 port=5432 user=postgres dbname=app", []string{"10.0.0.5"}, "5432", "postgres"},
		{"postgres:///app", nil, "", ""},
	}
	for _, tc := range cases {
		endpoint, err := ParseEndpoint(tc.raw)
		if err != nil {
			t.Fatalf("ParseEndpoint(%q): %v", tc.raw, err)
		}
		if strings.Join(endpoint.Hosts, ",") != strings.Join(tc.hosts, ",") || endpoint.Port != tc.port || endpoint.User != tc.user {
			t.Errorf("ParseEndpoint(%q) = %+v, want hosts %v port %q user %q", tc.raw, endpoint, tc.hosts, tc.port, tc.user)
		}
	}
}

func TestPrivateHost(t *testing.T) {
	lookup := func(host string) ([]net.IP, error) {
		switch host {
		case "db.corp.example":
			return []net.IP{net.ParseIP("10.1.2.3")}, nil
		case "public.example":
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		}
		return nil, errors.New("no such host")
	}
	private := map[string]string{
		"postgres://u@localhost/app":                "loopback",
		"postgres://u@127.0.0.1:55432/app":          "loopback",
		"postgres://u@[::1]/app":                    "loopback",
		"postgres://u@192.168.1.20/app":             "private-network",
		"postgres://u@172.16.0.9/app":               "private-network",
		"postgres://u@[fd00::1]/app":                "private-network",
		"postgres://u@100.101.102.103/app":          "CGNAT",
		"postgres://u@169.254.169.254/app":          "metadata",
		"postgres://u@db.corp.example/app":          "resolves to 10.1.2.3",
		"postgres://u@postgres.internal/app":        "internal",
		"postgres://u@nas.local/app":                "mDNS",
		"postgres:///app":                           "unix socket",
		"host=/tmp dbname=app":                      "unix-socket",
		"postgres://u@[::ffff:10.0.0.1]/app":        "private-network",
		"postgres://u@public.example,localhost/app": "loopback",
	}
	for raw, want := range private {
		endpoint, err := ParseEndpoint(raw)
		if err != nil {
			t.Fatalf("ParseEndpoint(%q): %v", raw, err)
		}
		_, reason, ok := PrivateHost(endpoint, lookup)
		if !ok || !strings.Contains(reason, want) {
			t.Errorf("PrivateHost(%q) = %q, %v; want a reason mentioning %q", raw, reason, ok, want)
		}
	}
	for _, raw := range []string{"postgres://u@public.example/app", "postgres://u@unresolvable.example/app", "postgres://u@203.0.113.7/app"} {
		endpoint, _ := ParseEndpoint(raw)
		if host, reason, ok := PrivateHost(endpoint, lookup); ok {
			t.Errorf("PrivateHost(%q) flagged %q: %s", raw, host, reason)
		}
	}
}

func TestRedactedMasksURLPassword(t *testing.T) {
	raw := "postgres://app:s3cret@localhost:5432/app"
	endpoint, _ := ParseEndpoint(raw)
	if got := Redacted(raw, endpoint); strings.Contains(got, "s3cret") || !strings.Contains(got, "localhost:5432") {
		t.Fatalf("Redacted = %q", got)
	}
	dsn := "host=localhost password=s3cret"
	endpoint, _ = ParseEndpoint(dsn)
	if got := Redacted(dsn, endpoint); strings.Contains(got, "s3cret") {
		t.Fatalf("Redacted leaked the DSN password: %q", got)
	}
}

func TestParseMajor(t *testing.T) {
	cases := map[string]int{
		"PostgreSQL 17.4 on x86_64-pc-linux-gnu, compiled by gcc": 17,
		"17.4 (Debian 17.4-1.pgdg120+2)":                          17,
		"pg_dump (PostgreSQL) 16.9 (Homebrew)":                    16,
		"170004":                                                  17,
		"90624":                                                   9,
		"18beta1":                                                 18,
	}
	for input, want := range cases {
		if got, ok := ParseMajor(input); !ok || got != want {
			t.Errorf("ParseMajor(%q) = %d, %v; want %d", input, got, ok, want)
		}
	}
	if _, ok := ParseMajor("unknown"); ok {
		t.Error("ParseMajor accepted a string without a version")
	}
}

func TestPgDumpAdvice(t *testing.T) {
	if got := PgDumpAdvice(17, 17); got != "" {
		t.Errorf("matching majors gave advice: %q", got)
	}
	older := PgDumpAdvice(16, 17)
	if !strings.Contains(older, "refuses") || !strings.Contains(older, "postgres:17") {
		t.Errorf("older pg_dump advice = %q", older)
	}
	newer := PgDumpAdvice(18, 17)
	if !strings.Contains(newer, "the dump works") || !strings.Contains(newer, "postgres:17") {
		t.Errorf("newer pg_dump advice = %q", newer)
	}
}

func TestDetectSupabase(t *testing.T) {
	endpoint, _ := ParseEndpoint("postgres://postgres:p@db.abcdefghijklmnop.supabase.co:5432/postgres")
	src, ok := DetectSupabase(endpoint)
	if !ok || !src.Direct || src.Ref != "abcdefghijklmnop" {
		t.Fatalf("direct host: %+v, %v", src, ok)
	}
	endpoint, _ = ParseEndpoint("postgres://postgres.abcdefghijklmnop:p@aws-0-eu-west-1.pooler.supabase.com:5432/postgres")
	src, ok = DetectSupabase(endpoint)
	if !ok || src.Direct || src.Ref != "abcdefghijklmnop" || src.PoolerHost != "aws-0-eu-west-1.pooler.supabase.com" {
		t.Fatalf("pooler host: %+v, %v", src, ok)
	}
	endpoint, _ = ParseEndpoint("postgres://u:p@db.example.com/app")
	if _, ok := DetectSupabase(endpoint); ok {
		t.Fatal("a non-Supabase host was detected as Supabase")
	}
}

func TestFetchSupabasePrimaryPoolerAndAdvice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/abcdefghijklmnop/config/database/pooler" || r.Header.Get("Authorization") != "Bearer sbp_test" {
			http.Error(w, "unexpected", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[
		  {"database_type":"READ_REPLICA","db_host":"aws-0-us-east-1.pooler.supabase.com","db_port":6543,"db_user":"postgres.abcdefghijklmnop","db_name":"postgres","pool_mode":"transaction"},
		  {"database_type":"PRIMARY","db_host":"aws-1-eu-west-1.pooler.supabase.com","db_port":6543,"db_user":"postgres.abcdefghijklmnop","db_name":"postgres","pool_mode":"transaction"}
		]`))
	}))
	defer server.Close()

	pooler, err := FetchSupabasePrimaryPooler(context.Background(), server.Client(), server.URL, "sbp_test", "abcdefghijklmnop")
	if err != nil {
		t.Fatalf("FetchSupabasePrimaryPooler: %v", err)
	}
	if pooler.DBHost != "aws-1-eu-west-1.pooler.supabase.com" {
		t.Fatalf("picked %+v, want the PRIMARY entry", pooler)
	}

	if _, err := FetchSupabasePrimaryPooler(context.Background(), server.Client(), server.URL, "wrong", "abcdefghijklmnop"); err == nil {
		t.Fatal("expected an error for a rejected token")
	}

	wrongPrefix := SupabaseSource{Ref: "abcdefghijklmnop", PoolerHost: "aws-0-eu-west-1.pooler.supabase.com", Port: "5432"}
	advice := strings.Join(SupabaseAdvice(wrongPrefix, &pooler), "\n")
	if !strings.Contains(advice, "aws-1-eu-west-1.pooler.supabase.com:5432") || !strings.Contains(advice, "not aws-0-eu-west-1") {
		t.Fatalf("wrong-prefix advice = %q", advice)
	}
	rightHost := SupabaseSource{Ref: "abcdefghijklmnop", PoolerHost: "aws-1-eu-west-1.pooler.supabase.com", Port: "5432"}
	if advice := SupabaseAdvice(rightHost, &pooler); len(advice) != 0 {
		t.Fatalf("correct session pooler still got advice: %q", advice)
	}
	direct := SupabaseSource{Ref: "abcdefghijklmnop", Direct: true}
	if advice := strings.Join(SupabaseAdvice(direct, &pooler), "\n"); !strings.Contains(advice, "IPv6-only") || !strings.Contains(advice, "aws-1-eu-west-1.pooler.supabase.com:5432") {
		t.Fatalf("direct-host advice = %q", advice)
	}
	if advice := strings.Join(SupabaseAdvice(wrongPrefix, nil), "\n"); !strings.Contains(advice, "SUPABASE_ACCESS_TOKEN") || !strings.Contains(advice, "Session pooler") {
		t.Fatalf("no-token advice = %q", advice)
	}
}

func TestEnvSourceURLSkipsCapyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "DATABASE_URL=\"postgres://u:p@shop.db.capydb.dev:6432/db\"\nexport DIRECT_URL='postgres://u:p@db.abcdefghijklmnop.supabase.co:5432/postgres'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	key, value, err := EnvSourceURL(path)
	if err != nil || key != "DIRECT_URL" || !strings.Contains(value, "supabase.co") {
		t.Fatalf("EnvSourceURL = %q, %q, %v", key, value, err)
	}
	if key, _, err := EnvSourceURL(filepath.Join(dir, "missing")); err != nil || key != "" {
		t.Fatalf("missing file: %q, %v", key, err)
	}
}
