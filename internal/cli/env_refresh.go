package cli

import (
	"slices"
	"strings"

	"github.com/capydatabase/capydb-cli/internal/envfile"
	"github.com/capydatabase/capydb-cli/internal/source"
)

// refreshResolver overwrites an existing env value silently when it points at
// the same database host as the incoming one - a CapyDB value refreshed after
// a credential rotation, which is what `env pull` exists for - and hands any
// other conflict to fallback. A key the CLI newly writes (DIRECT_URL, say)
// can already hold the user's value for another provider; that must not be
// replaced without a word.
func refreshResolver(fallback envfile.ConflictResolver) envfile.ConflictResolver {
	return func(key, existing, incoming string) (bool, error) {
		if sameDatabaseHost(existing, incoming) {
			return true, nil
		}
		return fallback(key, existing, incoming)
	}
}

func sameDatabaseHost(left, right string) bool {
	leftEndpoint, err := source.ParseEndpoint(left)
	if err != nil || len(leftEndpoint.Hosts) == 0 {
		return false
	}
	rightEndpoint, err := source.ParseEndpoint(right)
	if err != nil {
		return false
	}
	return slices.EqualFunc(leftEndpoint.Hosts, rightEndpoint.Hosts, strings.EqualFold)
}
