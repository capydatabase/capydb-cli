package source

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Platform majors offered for new CapyDB databases. KEEP IN LOCKSTEP with
// backend service.SupportedPostgresVersions.
const (
	MinSupportedMajor = 16
	MaxSupportedMajor = 18
)

var versionPattern = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// ParseMajor extracts the Postgres major from any of the shapes a version
// arrives in: `select version()` ("PostgreSQL 17.4 on x86_64..."),
// `show server_version` ("17.4 (Debian 17.4-1.pgdg120+2)"),
// `pg_dump --version` ("pg_dump (PostgreSQL) 17.4"), and
// server_version_num ("170004"). Pre-10 two-part majors ("9.6") are returned
// as 9.
func ParseMajor(version string) (int, bool) {
	version = strings.TrimSpace(version)
	if version == "" {
		return 0, false
	}
	if number, err := strconv.Atoi(version); err == nil && number >= 10000 {
		// server_version_num: MMmmmm from 10 on (170004), Mmmpp before
		// (90624); dividing by 10000 yields the major for both.
		return number / 10000, true
	}
	match := versionPattern.FindStringSubmatch(version)
	if match == nil {
		return 0, false
	}
	major, err := strconv.Atoi(match[1])
	if err != nil || major <= 0 {
		return 0, false
	}
	return major, true
}

// LocalPgDumpMajor runs `pg_dump --version` and returns its major. ok is false
// when pg_dump is not on PATH or its output is unrecognisable - the caller
// then has nothing to compare and says nothing.
func LocalPgDumpMajor(ctx context.Context) (int, bool) {
	path, err := exec.LookPath("pg_dump")
	if err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return 0, false
	}
	return ParseMajor(string(output))
}

// ServerMajor asks a live source for its major with a read-only query.
func ServerMajor(ctx context.Context, db *sql.DB) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var version string
	if err := db.QueryRowContext(ctx, "select current_setting('server_version_num')").Scan(&version); err != nil {
		return 0, fmt.Errorf("read source server version: %w", err)
	}
	major, ok := ParseMajor(version)
	if !ok {
		return 0, fmt.Errorf("unrecognised source server version %q", version)
	}
	return major, nil
}

// PgDumpAdvice explains a local pg_dump / source server major mismatch, or
// returns "" when the majors match. The two directions fail differently:
// an older pg_dump refuses to dump a newer server outright, while a newer one
// dumps fine but can write settings an older restore target rejects (pg_dump
// 17 emits SET transaction_timeout, which 16 does not know).
func PgDumpAdvice(localMajor, sourceMajor int) string {
	pinned := fmt.Sprintf("docker run --rm postgres:%d pg_dump", sourceMajor)
	switch {
	case localMajor == sourceMajor:
		return ""
	case localMajor < sourceMajor:
		return fmt.Sprintf("your pg_dump is %d but the source runs Postgres %d: pg_dump refuses to dump a newer server (\"server version mismatch\"). Use a matching client, e.g. `%s ...`.", localMajor, sourceMajor, pinned)
	default:
		return fmt.Sprintf("your pg_dump is %d but the source runs Postgres %d: the dump works, but it can contain settings a Postgres %d restore target rejects. Pin the client to the source major, e.g. `%s ...`.", localMajor, sourceMajor, sourceMajor, pinned)
	}
}
