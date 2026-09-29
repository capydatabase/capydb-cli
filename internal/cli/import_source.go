package cli

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/source"
)

// Seams for tests: DNS for the private-host check, the Supabase Management
// API base URL, and the source server's major.
var (
	sourceLookup       source.Lookup = net.LookupIP
	supabaseAPIBaseURL               = source.SupabaseAPIURL
	sourceServerMajor                = probeSourceServerMajor
	localPgDumpMajor                 = source.LocalPgDumpMajor
)

// guardPrivateImportSource stops an import whose source CapyDB cannot reach -
// loopback, a unix socket, RFC 1918 / ULA, CGNAT, link-local - before any API
// call, and says what works instead: dump it on this machine and upload the
// file. The control plane would reject the same URL with a bare 400.
//
// Because such a source IS reachable from here, this is also where the local
// pg_dump is compared with the source's major: the user is about to run it.
func guardPrivateImportSource(cmd *cobra.Command, sourceURL, projectRef, command string) error {
	endpoint, err := source.ParseEndpoint(sourceURL)
	if err != nil {
		// Not a shape this parser knows; the control plane validates it.
		return nil
	}
	host, reason, private := source.PrivateHost(endpoint, sourceLookup)
	if !private {
		return nil
	}

	subject := "this source"
	if host != "" {
		subject = fmt.Sprintf("%q", host)
	}
	var message strings.Builder
	fmt.Fprintf(&message, "CapyDB cannot reach %s: %s. Imports run from CapyDB's network, so a source on this machine or a private network has to come in as a dump file.\n", subject, reason)
	switch command {
	case "follow":
		message.WriteString("A --follow import streams changes from the source until cutover, so it needs a source CapyDB can reach; for this one take a one-off dump instead:\n\n")
	case "preflight":
		message.WriteString("The preflight also connects from CapyDB. To check this source locally run `capydb migrate scan --source-url ...`; to import it, dump it here and upload the file:\n\n")
	default:
		message.WriteString("Dump it here and upload the file:\n\n")
	}
	target := ""
	if strings.TrimSpace(projectRef) != "" {
		target = " --project " + strings.TrimSpace(projectRef)
	}
	fmt.Fprintf(&message, "  pg_dump -Fc --no-owner --no-privileges -d '%s' -f source.dump\n", source.Redacted(sourceURL, endpoint))
	fmt.Fprintf(&message, "  capydb import --file source.dump%s\n", target)
	if endpoint.IsURL {
		message.WriteString("\n(The password is masked above; use your full connection string.)")
	}
	if advice := pgDumpAdviceForSource(cmd.Context(), sourceURL); advice != "" {
		message.WriteString("\n" + advice)
	}
	return usageErrorf("%s", strings.TrimRight(message.String(), "\n"))
}

// pgDumpAdviceForSource compares the local pg_dump with the live source's
// major. It returns "" when they match or when there is nothing to compare.
func pgDumpAdviceForSource(ctx context.Context, sourceURL string) string {
	sourceMajor, err := sourceServerMajor(ctx, sourceURL)
	if err != nil {
		return "Could not read the source's Postgres version (" + err.Error() + "); run `select version()` on it and use a pg_dump of the same major."
	}
	localMajor, ok := localPgDumpMajor(ctx)
	if !ok {
		return fmt.Sprintf("pg_dump is not on PATH; install the Postgres %d client tools, or run `docker run --rm --network host postgres:%d pg_dump ...`.", sourceMajor, sourceMajor)
	}
	if advice := source.PgDumpAdvice(localMajor, sourceMajor); advice != "" {
		return "Note: " + advice
	}
	return ""
}

func probeSourceServerMajor(ctx context.Context, sourceURL string) (int, error) {
	db, err := sql.Open("pgx", sourceURL)
	if err != nil {
		return 0, fmt.Errorf("open source database: %w", err)
	}
	defer func() { _ = db.Close() }()
	return source.ServerMajor(ctx, db)
}

// noteSupabaseSource prints Supabase connection advice for a source URL: the
// session pooler host the Management API reports for the project (the aws-0 /
// aws-1 prefix differs per project), read when SUPABASE_ACCESS_TOKEN is set.
// Without a token the generic advice is printed only when verbose (the
// preflight), since it cannot tell whether the string is already right.
func noteSupabaseSource(cmd *cobra.Command, sourceURL string, verbose bool) {
	endpoint, err := source.ParseEndpoint(sourceURL)
	if err != nil {
		return
	}
	src, ok := source.DetectSupabase(endpoint)
	if !ok {
		return
	}
	errOut := cmd.ErrOrStderr()

	var pooler *source.SupabasePooler
	if token := strings.TrimSpace(os.Getenv("SUPABASE_ACCESS_TOKEN")); token != "" && src.Ref != "" {
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		fetched, fetchErr := source.FetchSupabasePrimaryPooler(ctx, http.DefaultClient, supabaseAPIBaseURL, token, src.Ref)
		if fetchErr != nil {
			_, _ = fmt.Fprintf(errOut, "note: could not read the Supabase pooler config: %v\n", fetchErr)
		} else {
			pooler = &fetched
		}
	}
	if pooler == nil && !verbose && !src.Direct {
		return
	}
	for _, line := range source.SupabaseAdvice(src, pooler) {
		_, _ = fmt.Fprintf(errOut, "note: %s\n", line)
	}
}

// notePgDumpForPreflight compares the local pg_dump with the source major the
// control plane's preflight measured. The server-side import uses its own
// client, so this only matters for a local dump (`capydb import --file`), and
// says so.
func notePgDumpForPreflight(cmd *cobra.Command, serverVersion string) {
	sourceMajor, ok := source.ParseMajor(serverVersion)
	if !ok {
		return
	}
	localMajor, ok := localPgDumpMajor(cmd.Context())
	if !ok {
		return
	}
	if advice := source.PgDumpAdvice(localMajor, sourceMajor); advice != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: if you dump locally for `capydb import --file`, %s\n", advice)
	}
}
