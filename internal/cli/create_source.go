package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/capydatabase/capydb-cli/internal/source"
)

// createPostgresVersion picks the major for a new project that will receive
// an import. sourceMajor is the source's major, 0 when unknown.
//
// Postgres restores upward, never downward: a dump restores into the same or
// a newer major, and pg_restore does not support an older one. So with no
// explicit --postgres-version the project gets the source's major, raised to
// the oldest one CapyDB offers; a source newer than every offered major is an
// error, because no project could take its data. An explicit
// --postgres-version always wins and only earns a warning.
func createPostgresVersion(requested string, sourceMajor int) (string, []string, error) {
	requested = strings.TrimSpace(requested)
	if sourceMajor <= 0 {
		return requested, nil, nil
	}

	if requested == "" {
		switch {
		case sourceMajor < source.MinSupportedMajor:
			return strconv.Itoa(source.MinSupportedMajor), []string{fmt.Sprintf(
				"The source runs Postgres %d; the oldest major CapyDB offers is %d, so the project gets %d. Restoring into a newer major works; dump with pg_dump %d or newer.",
				sourceMajor, source.MinSupportedMajor, source.MinSupportedMajor, source.MinSupportedMajor)}, nil
		case sourceMajor > source.MaxSupportedMajor:
			return "", nil, usageErrorf(
				"the source runs Postgres %d, newer than the newest major CapyDB offers (%d), and a dump does not restore into an older major; pass --postgres-version to create the project anyway",
				sourceMajor, source.MaxSupportedMajor)
		default:
			return strconv.Itoa(sourceMajor), []string{fmt.Sprintf(
				"Using Postgres %d to match the source (pass --postgres-version to choose another).", sourceMajor)}, nil
		}
	}

	requestedMajor, ok := source.ParseMajor(requested)
	switch {
	case !ok || requestedMajor == sourceMajor:
		return requested, nil, nil
	case requestedMajor < sourceMajor:
		return requested, []string{fmt.Sprintf(
			"warning: --postgres-version %d is older than the source (Postgres %d). A dump does not restore into an older major, so importing this source will fail; use --postgres-version %d.",
			requestedMajor, sourceMajor, sourceMajor)}, nil
	default:
		return requested, []string{fmt.Sprintf(
			"note: --postgres-version %d is newer than the source (Postgres %d). The import upgrades the data; dump it with pg_dump %d.",
			requestedMajor, sourceMajor, requestedMajor)}, nil
	}
}

// resolveCreateSourceMajor reads the major of --source-url, or, without one,
// points at a non-CapyDB database URL already in the env file the project is
// about to be linked to. The env file is only read, never connected to: probing
// a database the user did not name is not this command's call.
func resolveCreateSourceMajor(ctx context.Context, progress io.Writer, sourceURL, requestedVersion, envPath string) (int, error) {
	if sourceURL = strings.TrimSpace(sourceURL); sourceURL != "" {
		major, err := sourceServerMajor(ctx, sourceURL)
		if err != nil {
			return 0, fmt.Errorf("--source-url: %w", err)
		}
		return major, nil
	}
	if strings.TrimSpace(requestedVersion) != "" {
		return 0, nil
	}
	key, _, err := source.EnvSourceURL(envPath)
	if err != nil || key == "" {
		// An unreadable env file is reported by the env write that follows
		// project creation; the hint is optional.
		return 0, nil
	}
	_, _ = fmt.Fprintf(progress,
		"note: %s sets %s to a database outside CapyDB. If you will import it, the project's Postgres major should match it: `capydb create --source-url <that URL>` picks it (this run uses the default).\n",
		filepath.Base(envPath), key)
	return 0, nil
}
