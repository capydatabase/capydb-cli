package cli

import "strings"

// quoteIdent renders a Postgres identifier (always quoted, so reserved words
// and mixed case survive).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// qualifiedName renders schema.name with both parts quoted.
func qualifiedName(schema, name string) string {
	return quoteIdent(schema) + "." + quoteIdent(name)
}
