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

// quoteLiteral renders a standard-conforming string literal.
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
