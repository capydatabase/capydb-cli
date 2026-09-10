package scan

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The RLS equivalence battery.
//
// Migrating a policy corpus is the part of a provider migration nobody can
// eyeball: 500 policies across 170 tables, and the failure mode is silent -
// correct-looking rows that belong to someone else. Reading the converted
// policies proves nothing, and neither does "it works when I log in".
//
// What does prove something is running the SAME reads under the SAME caller
// identities against both databases and diffing the answers. On the myroomiev3
// migration a 23-table x 5-context matrix came back byte-identical to the
// Supabase copy at every step, and that - not the policy diff - is what made
// the cutover safe to do.

// BatteryContext is one caller identity: a label and the JWT claims that
// identity presents. The claims are set transaction-locally, exactly as the
// application sets them at runtime.
type BatteryContext struct {
	Label  string         `json:"label"`
	Claims map[string]any `json:"claims"`
}

// BatteryCell is the outcome of reading one table as one context. Count is
// meaningful only when Error is empty; an error is itself a result worth
// comparing, because "denied on both sides" is equivalence too.
type BatteryCell struct {
	Context string `json:"context"`
	Table   string `json:"table"`
	// Mode is "read" (SELECT, the USING clause of the SELECT policy) or
	// "write" (SELECT ... FOR UPDATE, which additionally applies the UPDATE
	// policy's USING clause - verified on postgres:17, where a table whose
	// SELECT policy showed 2 rows showed 1 under FOR UPDATE).
	Mode  string `json:"mode"`
	Count int64  `json:"count"`
	Error string `json:"error,omitempty"`
}

// Key identifies the cell across the two databases.
func (c BatteryCell) Key() string { return c.Context + "\x00" + c.Table + "\x00" + c.Mode }

// Result renders the comparable value: a row count, or the SQLSTATE. The
// message is deliberately excluded - wording differs between majors and
// providers, the class does not.
func (c BatteryCell) Result() string {
	if c.Error != "" {
		return "ERR " + c.Error
	}
	return fmt.Sprintf("%d", c.Count)
}

// BatteryDivergence is one cell where the two databases disagree.
type BatteryDivergence struct {
	Context string `json:"context"`
	Table   string `json:"table"`
	Mode    string `json:"mode"`
	Source  string `json:"source"`
	Target  string `json:"target"`
}

// BatteryReport is the verdict.
type BatteryReport struct {
	Contexts     int                 `json:"contexts"`
	Tables       int                 `json:"tables"`
	Checks       int                 `json:"checks"`
	Divergences  []BatteryDivergence `json:"divergences"`
	SourceOnly   []string            `json:"source_only_tables"`
	TargetOnly   []string            `json:"target_only_tables"`
	Inconclusive []string            `json:"inconclusive,omitempty"`
}

// Equivalent reports whether every comparable cell matched.
func (r BatteryReport) Equivalent() bool {
	return len(r.Divergences) == 0 && len(r.SourceOnly) == 0 && len(r.TargetOnly) == 0
}

// DiffBattery compares two runs. Pure, so the comparison is testable without
// databases - which matters, because this function deciding "equivalent" is
// the whole safety claim.
func DiffBattery(source, target []BatteryCell) BatteryReport {
	sourceByKey := make(map[string]BatteryCell, len(source))
	for _, cell := range source {
		sourceByKey[cell.Key()] = cell
	}
	targetByKey := make(map[string]BatteryCell, len(target))
	for _, cell := range target {
		targetByKey[cell.Key()] = cell
	}

	report := BatteryReport{
		Divergences: []BatteryDivergence{},
		SourceOnly:  []string{},
		TargetOnly:  []string{},
	}

	contexts := map[string]bool{}
	tables := map[string]bool{}
	for _, cell := range source {
		contexts[cell.Context] = true
		tables[cell.Table] = true
	}

	for key, sourceCell := range sourceByKey {
		targetCell, ok := targetByKey[key]
		if !ok {
			report.SourceOnly = append(report.SourceOnly, sourceCell.Table)
			continue
		}
		report.Checks++
		if sourceCell.Result() != targetCell.Result() {
			report.Divergences = append(report.Divergences, BatteryDivergence{
				Context: sourceCell.Context,
				Table:   sourceCell.Table,
				Mode:    sourceCell.Mode,
				Source:  sourceCell.Result(),
				Target:  targetCell.Result(),
			})
		}
	}
	for key, targetCell := range targetByKey {
		if _, ok := sourceByKey[key]; !ok {
			report.TargetOnly = append(report.TargetOnly, targetCell.Table)
		}
	}

	report.Contexts = len(contexts)
	report.Tables = len(tables)
	report.SourceOnly = dedupeSorted(report.SourceOnly)
	report.TargetOnly = dedupeSorted(report.TargetOnly)
	sort.Slice(report.Divergences, func(i, j int) bool {
		if report.Divergences[i].Table != report.Divergences[j].Table {
			return report.Divergences[i].Table < report.Divergences[j].Table
		}
		return report.Divergences[i].Context < report.Divergences[j].Context
	})
	return report
}

func dedupeSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// ListRLSTables returns the RLS-enabled tables in the app schemas - the only
// tables where the answer can differ by caller, and therefore the only ones
// worth putting in the battery.
func ListRLSTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		select c.relname
		from pg_catalog.pg_class c
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace
		where c.relkind = 'r' and c.relrowsecurity
		  and n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
		order by 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// RunBattery reads every table as every context and returns one cell per pair.
//
// Each (context, table) pair runs in its OWN transaction, rolled back. That is
// not tidiness: claims are transaction-local, so the transaction IS the
// identity boundary, and rolling back guarantees the battery cannot alter the
// database it is auditing - including the production source.
func RunBattery(ctx context.Context, db *sql.DB, contexts []BatteryContext, tables []string, probeWrites bool) ([]BatteryCell, error) {
	modes := []string{"read"}
	if probeWrites {
		modes = append(modes, "write")
	}
	cells := make([]BatteryCell, 0, len(contexts)*len(tables)*len(modes))
	for _, batteryContext := range contexts {
		claims, err := json.Marshal(batteryContext.Claims)
		if err != nil {
			return nil, fmt.Errorf("encode claims for %q: %w", batteryContext.Label, err)
		}
		for _, table := range tables {
			for _, mode := range modes {
				cell := BatteryCell{Context: batteryContext.Label, Table: table, Mode: mode}
				cell.Count, cell.Error = readAsContext(ctx, db, string(claims), table, mode)
				cells = append(cells, cell)
			}
		}
	}
	return cells, nil
}

// readAsContext runs one count under one identity, returning the SQLSTATE
// instead of the message when it fails: a policy that denies is a result, not
// an outage, and the two databases must deny identically.
func readAsContext(ctx context.Context, db *sql.DB, claims, table, mode string) (int64, string) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, sqlStateOf(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "select set_config('request.jwt.claims', $1, true)", claims); err != nil {
		return 0, sqlStateOf(err)
	}
	var count int64
	// The table name comes from the catalog, never from user input, and is
	// quoted regardless.
	query := "select count(*) from " + quoteIdentifier(table)
	if mode == "write" {
		// FOR UPDATE additionally applies the UPDATE policy's USING clause, so
		// this measures write reachability without writing anything. It takes
		// row locks for the life of the transaction, which is why the caller
		// makes it opt-in - and why the transaction is rolled back immediately.
		query = "with capydb_probe as (select 1 from " + quoteIdentifier(table) +
			" for update) select count(*) from capydb_probe"
	}
	if err := tx.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, sqlStateOf(err)
	}
	return count, ""
}

// sqlStateOf extracts the SQLSTATE for comparison, falling back to a compact
// message when the driver did not supply one.
func sqlStateOf(err error) string {
	type sqlStater interface{ SQLState() string }
	if stater, ok := err.(sqlStater); ok && stater.SQLState() != "" {
		return stater.SQLState()
	}
	message := compactError(err)
	if index := strings.IndexByte(message, ':'); index > 0 && index < 12 {
		return message
	}
	return message
}

// ParseBatteryContexts reads the contexts file: a JSON array of
// {"label": "...", "claims": {...}}. An empty claims object is the anonymous
// caller, which is worth including - it is the context most likely to regress.
func ParseBatteryContexts(raw []byte) ([]BatteryContext, error) {
	var contexts []BatteryContext
	if err := json.Unmarshal(raw, &contexts); err != nil {
		return nil, fmt.Errorf("parse contexts: %w", err)
	}
	if len(contexts) == 0 {
		return nil, fmt.Errorf("contexts file defines no contexts")
	}
	for index, batteryContext := range contexts {
		if strings.TrimSpace(batteryContext.Label) == "" {
			return nil, fmt.Errorf("context %d has no label", index)
		}
	}
	return contexts, nil
}
