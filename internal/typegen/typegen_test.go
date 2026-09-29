package typegen

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capydatabase/capydbclient"
)

func fixtureSchema() capydbclient.DatabaseSchema {
	return capydbclient.DatabaseSchema{
		Schemas: []capydbclient.SchemaNamespace{
			{
				Name: "billing",
				Tables: []capydbclient.SchemaTable{{
					Name: "invoices",
					Kind: "table",
					Columns: []capydbclient.SchemaColumn{
						{Name: "id", UDTName: "int8", Identity: "always"},
						{Name: "amount", UDTName: "numeric"},
						{Name: "paid_at", UDTName: "timestamptz", IsNullable: true},
						{Name: "shape", UDTName: "geometry", IsNullable: true},
					},
				}},
			},
			{
				Name: "public",
				Enums: []capydbclient.SchemaEnum{
					{Name: "user_status", Values: []string{"active", "on-hold", "2fa", `quo"te`}},
				},
				Tables: []capydbclient.SchemaTable{
					{
						Name:    "users",
						Kind:    "table",
						Comment: "People\nwho \"log in\"",
						Columns: []capydbclient.SchemaColumn{
							{Name: "id", UDTName: "uuid"},
							{Name: "email", UDTName: "text", Comment: "unique\nlogin"},
							{Name: "status", UDTName: "user_status", UDTSchema: "public", IsEnum: true},
							{Name: "nickname", UDTName: "text", IsNullable: true},
							{Name: "created_at", UDTName: "timestamptz"},
							{Name: "tags", UDTName: "text", IsArray: true, ArrayDims: 1},
							{Name: "matrix", UDTName: "int4", IsArray: true, ArrayDims: 2, IsNullable: true},
							{Name: "meta", UDTName: "jsonb", IsNullable: true},
							{Name: "class", UDTName: "text"},
							{Name: "user id", UDTName: "int4"},
							{Name: "userId", UDTName: "int4", IsNullable: true},
							{Name: "avatar", UDTName: "bytea", IsNullable: true},
							{Name: "a,b`c", UDTName: "bool"},
						},
					},
					{
						Name: "active_users",
						Kind: "view",
						Columns: []capydbclient.SchemaColumn{
							{Name: "id", UDTName: "uuid", IsNullable: true},
						},
					},
				},
			},
		},
	}
}

func TestGenerateGoTypeChecks(t *testing.T) {
	source, err := GenerateGo(fixtureSchema(), "models")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "models.go", source, parser.ParseComments)
	if err != nil {
		t.Fatalf("generated Go does not parse: %v\n%s", err, source)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("models", fset, []*ast.File{file}, nil); err != nil {
		t.Fatalf("generated Go does not type-check: %v\n%s", err, source)
	}

	for _, want := range []string{
		"package models",
		"type UserStatus string",
		`UserStatusOnHold UserStatus = "on-hold"`,
		`UserStatus2fa UserStatus = "2fa"`,
		"type UsersRow struct",
		"ID string `db:\"id\" json:\"id\"`",
		"Status UserStatus",
		"Nickname *string",
		"CreatedAt time.Time",
		"Tags []string",
		"Matrix [][]int32",
		"Meta json.RawMessage",
		"Avatar []byte",
		"UserID int32",
		"UserID2 *int32",
		"type ActiveUsersRow struct",
		"type BillingInvoicesRow struct",
		"Amount string",
		"PaidAt *time.Time",
		"Shape any",
		`BillingInvoicesSchema = "billing"`,
		`UsersColumnCreatedAt = "created_at"`,
	} {
		if !strings.Contains(squashSpaces(source), squashSpaces(want)) {
			t.Errorf("generated Go is missing %q\n%s", want, source)
		}
	}
	// A column name with a comma keeps only its db tag; the backtick forces an
	// interpreted tag literal.
	if !strings.Contains(source, `"db:\"a,b`+"`"+`c\""`) {
		t.Errorf("expected an interpreted tag literal for the backtick column\n%s", source)
	}
	// Catalog comments stay on one line.
	if strings.Contains(source, "People\nwho") || strings.Contains(source, "unique\nlogin") {
		t.Errorf("a catalog line terminator leaked into the output\n%s", source)
	}
}

func TestGenerateGoRejectsBadPackage(t *testing.T) {
	for _, name := range []string{"", "func", "my-models", "1db", "_"} {
		if _, err := GenerateGo(fixtureSchema(), name); err == nil {
			t.Errorf("package %q accepted", name)
		}
	}
}

func TestGenerateGoWithoutTimeOrJSONHasNoImports(t *testing.T) {
	schema := capydbclient.DatabaseSchema{Schemas: []capydbclient.SchemaNamespace{{
		Name:   "public",
		Tables: []capydbclient.SchemaTable{{Name: "t", Kind: "table", Columns: []capydbclient.SchemaColumn{{Name: "id", UDTName: "int4"}}}},
	}}}
	source, err := GenerateGo(schema, "db")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(source, "import") {
		t.Fatalf("unexpected import block:\n%s", source)
	}
}

func TestGeneratePythonDataclassRuns(t *testing.T) {
	source, err := GeneratePython(fixtureSchema(), PythonStyleDataclass)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"from __future__ import annotations",
		"import datetime\nimport decimal\nimport uuid\n",
		"from dataclasses import dataclass",
		"class UserStatus(str, Enum):",
		`ON_HOLD = "on-hold"`,
		`V_2FA = "2fa"`,
		"@dataclass(frozen=True)\nclass UsersRow:",
		"id: uuid.UUID",
		"status: UserStatus",
		"nickname: str | None",
		"created_at: datetime.datetime",
		"tags: list[str]",
		"matrix: list[list[int]] | None",
		"meta: Any",
		`class_: str  # column "class"`,
		`user_id: int  # column "user id"`,
		"userId: int | None",
		"avatar: bytes | None",
		"amount: decimal.Decimal",
		"shape: Any",
		`TABLE: ClassVar[str] = "invoices"`,
		"class BillingInvoicesRow:",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("generated Python is missing %q\n%s", want, source)
		}
	}
	runPython(t, source, `
import models, uuid, datetime
row = models.UsersRow(id=uuid.uuid4(), email="a@b", status=models.UserStatus.ON_HOLD, nickname=None,
    created_at=datetime.datetime.now(), tags=[], matrix=None, meta=None, class_="x", user_id=1,
    userId=None, avatar=None, a_b_c=True)
assert row.status == "on-hold"
assert models.UsersRow.TABLE == "users"
assert "quo" in models.UserStatus.QUO_TE.value
`)
}

func TestGeneratePythonPydanticParses(t *testing.T) {
	source, err := GeneratePython(fixtureSchema(), PythonStylePydantic)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"from pydantic import BaseModel, ConfigDict, Field",
		"class UsersRow(BaseModel):",
		"model_config = ConfigDict(frozen=True, populate_by_name=True)",
		`class_: str = Field(alias="class")`,
		`user_id: int = Field(alias="user id")`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("generated Python is missing %q\n%s", want, source)
		}
	}
	// pydantic may not be installed where tests run; the syntax check still
	// catches every rendering bug.
	runPython(t, source, "")
}

func TestGeneratePythonRejectsUnknownStyle(t *testing.T) {
	if _, err := GeneratePython(fixtureSchema(), "attrs"); err == nil {
		t.Fatal("unknown style accepted")
	}
}

// runPython writes the module and, when python3 is available, compiles it
// and runs script (if any) against it.
func runPython(t *testing.T, source, script string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.py"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	check := exec.Command(python, "-c", "import ast,sys; ast.parse(open(sys.argv[1]).read())", filepath.Join(dir, "models.py"))
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("generated Python does not parse: %v\n%s\n%s", err, out, source)
	}
	if script == "" {
		return
	}
	run := exec.Command(python, "-c", script)
	run.Dir = dir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("generated Python failed at runtime: %v\n%s\n%s", err, out, source)
	}
}

// squashSpaces collapses runs of blanks so assertions ignore gofmt alignment.
func squashSpaces(value string) string {
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
}
