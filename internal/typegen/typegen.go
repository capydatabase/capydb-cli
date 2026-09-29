// Package typegen renders the canonical schema document (GET
// /v1/projects/{id}/schema) as Go and Python source.
//
// The TypeScript, Zod and Drizzle generators live in the control plane
// (backend internal/service/typegen.go) so every surface shares one
// implementation. Go and Python are rendered here, client-side, until they
// move next to those: the package is deliberately self-contained - pure
// functions over the schema contract with no CLI dependencies - so the move is
// a file copy plus type renames. The row semantics mirror the backend's
// (nullable columns, enum lookup across namespaces, arrays with their declared
// dimensions, views as read-only row types).
package typegen

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/capydatabase/capydbclient"
)

// enumKey identifies an enum type across namespaces.
type enumKey struct {
	schema string
	name   string
}

func collectEnums(schema capydbclient.DatabaseSchema) map[enumKey]capydbclient.SchemaEnum {
	enums := make(map[enumKey]capydbclient.SchemaEnum)
	for _, namespace := range schema.Schemas {
		for _, enum := range namespace.Enums {
			enums[enumKey{schema: namespace.Name, name: enum.Name}] = enum
		}
	}
	return enums
}

// sortedNamespaces orders public first, then the rest by name - the order
// the backend generators use, so every language lists types the same way.
func sortedNamespaces(schema capydbclient.DatabaseSchema) []capydbclient.SchemaNamespace {
	namespaces := append([]capydbclient.SchemaNamespace{}, schema.Schemas...)
	sort.SliceStable(namespaces, func(i, j int) bool {
		if (namespaces[i].Name == "public") != (namespaces[j].Name == "public") {
			return namespaces[i].Name == "public"
		}
		return namespaces[i].Name < namespaces[j].Name
	})
	return namespaces
}

var identifierWordSplit = regexp.MustCompile(`[^A-Za-z0-9]+`)

func splitWords(name string) []string {
	words := []string{}
	for _, part := range identifierWordSplit.Split(name, -1) {
		if part != "" {
			words = append(words, part)
		}
	}
	if len(words) == 0 {
		return []string{"value"}
	}
	return words
}

// pascalCase is the backend's naming rule (user_accounts -> UserAccounts); a
// leading digit gets a "T" prefix so the result is always an identifier.
func pascalCase(name string) string {
	var b strings.Builder
	for _, word := range splitWords(name) {
		b.WriteString(strings.ToUpper(word[:1]))
		b.WriteString(strings.ToLower(word[1:]))
	}
	result := b.String()
	if result[0] >= '0' && result[0] <= '9' {
		return "T" + result
	}
	return result
}

// typePrefix disambiguates type names across namespaces: public keeps the
// bare name, other schemas are prefixed.
func typePrefix(schemaName string) string {
	if schemaName == "public" {
		return ""
	}
	return pascalCase(schemaName)
}

// commentText keeps catalog text on one source line. Identifiers and comments
// are user-controlled; a raw line terminator would let a crafted schema
// inject code into the generated file.
func commentText(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", " ", " ", " ", " ").Replace(value)
}

func isTableKind(kind string) bool {
	return kind == "table" || kind == "partitioned_table" || kind == "foreign_table"
}

func arrayDims(column capydbclient.SchemaColumn) int {
	if column.ArrayDims > 1 {
		return column.ArrayDims
	}
	return 1
}

// nameSet hands out unique names: a second request for a taken name gets a
// numeric suffix, so two identifiers that normalize alike (user_id, userId)
// never collide in the output.
type nameSet map[string]bool

func (s nameSet) claim(name string) string {
	candidate := name
	for i := 2; s[candidate]; i++ {
		candidate = name + strconv.Itoa(i)
	}
	s[candidate] = true
	return candidate
}
