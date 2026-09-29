package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// `capydb import --data-only` copies rows from a source database into the
// schema that already exists on the target (applied by migrations), table
// by table in foreign-key order.
//
// It runs on this machine: the CLI connects to the source and to the
// project's direct URL and streams COPY between them. That is what makes
// FK ordering possible without superuser (pg_dump emits tables
// alphabetically, and `pg_restore --disable-triggers` needs a superuser the
// project role is not), and it reaches sources CapyDB's network cannot
// (localhost, a private network).
//
// Guarantees:
//   - One snapshot: the source is read in a single REPEATABLE READ READ ONLY
//     transaction, so rows that reference each other are copied consistently.
//   - All or nothing: the target is written in one transaction.
//   - Parents before children, from the TARGET's foreign keys. A cycle is
//     loaded with its deferrable constraints deferred to commit; a cycle of
//     non-deferrable constraints stops before anything is written.
//   - The target tables must be empty; the import never merges.
//   - User triggers are disabled per table for the load (the source rows
//     already carry what those triggers computed; audit triggers would log
//     the copy itself). FORCE ROW LEVEL SECURITY is lifted per table for the
//     load. Both are restored inside the same transaction.
//   - Sequences behind serial/identity columns are set to the source's
//     position, so the next insert does not collide.
//   - Materialized views are refreshed and the copied tables analyzed after
//     the commit.

// dataOnlyTable is one table to copy.
type dataOnlyTable struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	// SourcePartitioned: the source relation is partitioned, so its rows
	// live in partitions and must be read without ONLY.
	SourcePartitioned bool  `json:"-"`
	ForceRLS          bool  `json:"-"`
	Rows              int64 `json:"rows,omitempty"`
}

func (t dataOnlyTable) key() string { return t.Schema + "." + t.Name }

func (t dataOnlyTable) qualified() string { return qualifiedName(t.Schema, t.Name) }

type dataOnlyPlan struct {
	Tables []dataOnlyTable `json:"tables"`
	// DeferConstraints is set when a foreign-key cycle is loaded by
	// deferring its (deferrable) constraints to commit.
	DeferConstraints  bool     `json:"defer_constraints"`
	MaterializedViews []string `json:"materialized_views"`
	Notes             []string `json:"notes"`
}

// fkEdge is a foreign key from child to parent.
type fkEdge struct {
	child, parent string
	deferrable    bool
}

// orderByForeignKeys returns tables with every parent before its children.
// Self-references need no order (a statement's own rows satisfy them at its
// end). Cycles are broken by dropping deferrable edges; if a cycle remains,
// the error names its tables. deferred reports whether any edge was dropped.
func orderByForeignKeys(tables []string, edges []fkEdge) (order []string, deferred bool, err error) {
	order, stuck := kahnOrder(tables, edges, false)
	if len(stuck) == 0 {
		return order, false, nil
	}
	order, stuck = kahnOrder(tables, edges, true)
	if len(stuck) == 0 {
		return order, true, nil
	}
	return nil, false, fmt.Errorf("foreign keys form a cycle between %s and none of its constraints is DEFERRABLE, so no load order satisfies them; "+
		"make one of them DEFERRABLE INITIALLY IMMEDIATE (ALTER TABLE ... ALTER CONSTRAINT ... DEFERRABLE) and run the import again",
		strings.Join(stuck, ", "))
}

// kahnOrder topologically sorts; stuck lists the tables left in cycles.
// Ready tables are taken alphabetically so the order is deterministic.
func kahnOrder(tables []string, edges []fkEdge, skipDeferrable bool) (order, stuck []string) {
	inSet := map[string]bool{}
	for _, table := range tables {
		inSet[table] = true
	}
	pending := map[string]int{}
	children := map[string][]string{}
	seen := map[[2]string]bool{}
	for _, edge := range edges {
		if edge.child == edge.parent || !inSet[edge.child] || !inSet[edge.parent] || (skipDeferrable && edge.deferrable) {
			continue
		}
		pair := [2]string{edge.child, edge.parent}
		if seen[pair] {
			continue
		}
		seen[pair] = true
		pending[edge.child]++
		children[edge.parent] = append(children[edge.parent], edge.child)
	}

	var ready []string
	for _, table := range tables {
		if pending[table] == 0 {
			ready = append(ready, table)
		}
	}
	sort.Strings(ready)
	for len(ready) > 0 {
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)
		for _, child := range children[next] {
			pending[child]--
			if pending[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(order) == len(tables) {
		return order, nil
	}
	placed := map[string]bool{}
	for _, table := range order {
		placed[table] = true
	}
	for _, table := range tables {
		if !placed[table] {
			stuck = append(stuck, table)
		}
	}
	sort.Strings(stuck)
	return order, stuck
}

// planDataOnlyImport decides what to copy, in which order, with which
// columns. It reads both catalogs and writes nothing.
func planDataOnlyImport(ctx context.Context, src, dst *pgx.Conn, schema api.DatabaseSchema) (dataOnlyPlan, error) {
	plan := dataOnlyPlan{Tables: []dataOnlyTable{}, MaterializedViews: []string{}, Notes: []string{}}

	candidates := map[string]dataOnlyTable{}
	var keys []string
	for _, namespace := range schema.Schemas {
		for _, table := range namespace.Tables {
			switch table.Kind {
			case "materialized_view":
				plan.MaterializedViews = append(plan.MaterializedViews, qualifiedName(namespace.Name, table.Name))
				continue
			case "table", "partitioned_table":
			default:
				continue
			}
			entry := dataOnlyTable{Schema: namespace.Name, Name: table.Name}

			sourceColumns, relkind, err := sourceRelation(ctx, src, entry.qualified())
			if err != nil {
				return dataOnlyPlan{}, err
			}
			if relkind == "" {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s does not exist in the source; left empty", entry.key()))
				continue
			}
			entry.SourcePartitioned = relkind == "p"

			targetColumns := map[string]bool{}
			for _, column := range table.Columns {
				targetColumns[column.Name] = true
				if column.IsGenerated {
					continue // computed on the target
				}
				if sourceColumns[column.Name] {
					entry.Columns = append(entry.Columns, column.Name)
					continue
				}
				if !column.IsNullable && column.Default == "" && column.Identity == "" {
					return dataOnlyPlan{}, fmt.Errorf("%s.%s is NOT NULL without a default on the target and missing from the source; add a default or align the schemas first", entry.key(), column.Name)
				}
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s.%s is not in the source; it gets its default", entry.key(), column.Name))
			}
			var dropped []string
			for column := range sourceColumns {
				if !targetColumns[column] {
					dropped = append(dropped, column)
				}
			}
			if len(dropped) > 0 {
				sort.Strings(dropped)
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s: source column(s) %s do not exist on the target and are not copied", entry.key(), strings.Join(dropped, ", ")))
			}
			if len(entry.Columns) == 0 {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s shares no columns with the source; left empty", entry.key()))
				continue
			}

			var nonEmpty, forceRLS bool
			if err := dst.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+entry.qualified()+"), (SELECT relforcerowsecurity FROM pg_class WHERE oid = $1::regclass)", entry.qualified()).Scan(&nonEmpty, &forceRLS); err != nil {
				return dataOnlyPlan{}, fmt.Errorf("inspect target %s: %w", entry.key(), err)
			}
			if nonEmpty {
				return dataOnlyPlan{}, fmt.Errorf("target table %s already has rows; --data-only loads into empty tables only (truncate it, or import into a fresh project)", entry.key())
			}
			entry.ForceRLS = forceRLS
			candidates[entry.key()] = entry
			keys = append(keys, entry.key())
		}
	}

	edges, err := targetForeignKeys(ctx, dst)
	if err != nil {
		return dataOnlyPlan{}, err
	}
	order, deferred, err := orderByForeignKeys(keys, edges)
	if err != nil {
		return dataOnlyPlan{}, err
	}
	plan.DeferConstraints = deferred
	for _, key := range order {
		plan.Tables = append(plan.Tables, candidates[key])
	}
	return plan, nil
}

// sourceRelation returns the source's columns and relkind for a relation, or
// an empty relkind when it does not exist.
func sourceRelation(ctx context.Context, src *pgx.Conn, qualified string) (map[string]bool, string, error) {
	rows, err := src.Query(ctx, `SELECT c.relkind::text, a.attname
  FROM pg_class c
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
 WHERE c.oid = to_regclass($1)`, qualified)
	if err != nil {
		return nil, "", fmt.Errorf("inspect source %s: %w", qualified, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	relkind := ""
	for rows.Next() {
		var kind, column string
		if err := rows.Scan(&kind, &column); err != nil {
			return nil, "", fmt.Errorf("inspect source %s: %w", qualified, err)
		}
		relkind = kind
		columns[column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("inspect source %s: %w", qualified, err)
	}
	return columns, relkind, nil
}

func targetForeignKeys(ctx context.Context, dst *pgx.Conn) ([]fkEdge, error) {
	// conparentid = 0 keeps the constraint declared on a partitioned table and
	// drops the per-partition clones Postgres derives from it.
	rows, err := dst.Query(ctx, `SELECT cn.nspname || '.' || c.relname, pn.nspname || '.' || p.relname, con.condeferrable
  FROM pg_constraint con
  JOIN pg_class c ON c.oid = con.conrelid
  JOIN pg_namespace cn ON cn.oid = c.relnamespace
  JOIN pg_class p ON p.oid = con.confrelid
  JOIN pg_namespace pn ON pn.oid = p.relnamespace
 WHERE con.contype = 'f' AND con.conparentid = 0`)
	if err != nil {
		return nil, fmt.Errorf("read target foreign keys: %w", err)
	}
	defer rows.Close()
	var edges []fkEdge
	for rows.Next() {
		var edge fkEdge
		if err := rows.Scan(&edge.child, &edge.parent, &edge.deferrable); err != nil {
			return nil, fmt.Errorf("read target foreign keys: %w", err)
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

// runDataOnlyCopy executes plan. progress receives one line per table.
func runDataOnlyCopy(ctx context.Context, src, dst *pgx.Conn, plan *dataOnlyPlan, progress io.Writer) (err error) {
	if _, err := src.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return fmt.Errorf("start source snapshot: %w", err)
	}
	defer func() { _, _ = src.Exec(context.Background(), "ROLLBACK") }()
	// The source may cap statements; one COPY of a large table is one
	// statement.
	if _, err := src.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("configure source: %w", err)
	}

	tx, err := dst.Begin(ctx)
	if err != nil {
		return fmt.Errorf("start target transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.Background())
		}
	}()
	setup := []string{"SET LOCAL statement_timeout = 0"}
	if plan.DeferConstraints {
		setup = append(setup, "SET CONSTRAINTS ALL DEFERRED")
	}
	for _, statement := range setup {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("configure target: %w", err)
		}
	}

	// Triggers and FORCE are switched off for every table before the first
	// row and back on after the last: Postgres refuses ALTER TABLE on a table
	// with pending (deferred) trigger events, so they cannot be toggled per
	// table while a cycle's constraints are deferred.
	for _, table := range plan.Tables {
		for _, statement := range loadToggles(table, false) {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("prepare %s: %w", table.key(), err)
			}
		}
	}
	for i := range plan.Tables {
		table := &plan.Tables[i]
		started := time.Now()
		rows, err := copyTable(ctx, src, tx.Conn(), *table)
		if err != nil {
			return fmt.Errorf("copy %s: %w", table.key(), err)
		}
		table.Rows = rows
		_, _ = fmt.Fprintf(progress, "  %s: %d rows (%s)\n", table.key(), rows, time.Since(started).Truncate(time.Millisecond))
	}

	if plan.DeferConstraints {
		// Check the deferred foreign keys now, so a violation names itself
		// before the commit.
		if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			return fmt.Errorf("foreign-key check: %w", err)
		}
	}
	for _, table := range plan.Tables {
		for _, statement := range loadToggles(table, true) {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("restore %s: %w", table.key(), err)
			}
		}
	}
	if err := syncSequences(ctx, src, tx.Conn(), plan.Tables); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// After the commit: failures here leave the data in place and are
	// reported, not fatal.
	for _, view := range plan.MaterializedViews {
		if _, err := dst.Exec(ctx, "REFRESH MATERIALIZED VIEW "+view); err != nil {
			plan.Notes = append(plan.Notes, fmt.Sprintf("refresh %s failed (run it yourself): %v", view, err))
		}
	}
	for _, table := range plan.Tables {
		if _, err := dst.Exec(ctx, "ANALYZE "+table.qualified()); err != nil {
			plan.Notes = append(plan.Notes, fmt.Sprintf("analyze %s failed: %v", table.key(), err))
		}
	}
	return nil
}

// copyTable streams one table from src to dst (inside dst's transaction).
func copyTable(ctx context.Context, src, dst *pgx.Conn, table dataOnlyTable) (int64, error) {
	columns := make([]string, len(table.Columns))
	for i, column := range table.Columns {
		columns[i] = quoteIdent(column)
	}
	columnList := strings.Join(columns, ", ")
	only := "ONLY "
	if table.SourcePartitioned {
		only = ""
	}

	reader, writer := io.Pipe()
	sourceDone := make(chan error, 1)
	go func() {
		_, err := src.PgConn().CopyTo(ctx, writer, "COPY (SELECT "+columnList+" FROM "+only+table.qualified()+") TO STDOUT")
		_ = writer.CloseWithError(err)
		sourceDone <- err
	}()
	tag, copyErr := dst.PgConn().CopyFrom(ctx, reader, "COPY "+table.qualified()+" ("+columnList+") FROM STDIN")
	_ = reader.CloseWithError(errors.Join(copyErr, io.ErrClosedPipe))
	sourceErr := <-sourceDone
	switch {
	case sourceErr != nil && !errors.Is(sourceErr, io.ErrClosedPipe):
		return 0, fmt.Errorf("read source: %w", sourceErr)
	case copyErr != nil:
		return 0, fmt.Errorf("write target: %w", copyErr)
	}

	return tag.RowsAffected(), nil
}

// loadToggles returns the statements that switch a table into load mode
// (user triggers off; FORCE ROW LEVEL SECURITY lifted, since COPY FROM refuses
// a table whose policies apply to the caller) or back out of it.
func loadToggles(table dataOnlyTable, restore bool) []string {
	name := table.qualified()
	if restore {
		statements := []string{"ALTER TABLE " + name + " ENABLE TRIGGER USER"}
		if table.ForceRLS {
			statements = append(statements, "ALTER TABLE "+name+" FORCE ROW LEVEL SECURITY")
		}
		return statements
	}
	statements := []string{"ALTER TABLE " + name + " DISABLE TRIGGER USER"}
	if table.ForceRLS {
		statements = append(statements, "ALTER TABLE "+name+" NO FORCE ROW LEVEL SECURITY")
	}
	return statements
}

// syncSequences moves each serial/identity sequence of the copied tables to
// the source's position.
func syncSequences(ctx context.Context, src, dst *pgx.Conn, tables []dataOnlyTable) error {
	for _, table := range tables {
		for _, column := range table.Columns {
			var targetSeq *string
			if err := dst.QueryRow(ctx, "SELECT pg_get_serial_sequence($1, $2)", table.qualified(), column).Scan(&targetSeq); err != nil {
				return fmt.Errorf("find sequence of %s.%s: %w", table.key(), column, err)
			}
			if targetSeq == nil {
				continue
			}
			var sourceSeq *string
			if err := src.QueryRow(ctx, "SELECT pg_get_serial_sequence($1, $2)", table.qualified(), column).Scan(&sourceSeq); err != nil {
				return fmt.Errorf("find source sequence of %s.%s: %w", table.key(), column, err)
			}
			if sourceSeq != nil {
				var lastValue int64
				var isCalled bool
				if err := src.QueryRow(ctx, "SELECT last_value, is_called FROM "+*sourceSeq).Scan(&lastValue, &isCalled); err != nil {
					return fmt.Errorf("read source sequence %s: %w", *sourceSeq, err)
				}
				if _, err := dst.Exec(ctx, "SELECT setval($1::regclass, $2, $3)", *targetSeq, lastValue, isCalled); err != nil {
					return fmt.Errorf("set sequence %s: %w", *targetSeq, err)
				}
				continue
			}
			// The source column had no sequence (values supplied by the app):
			// continue after the largest copied value.
			if _, err := dst.Exec(ctx, fmt.Sprintf("SELECT setval($1::regclass, COALESCE((SELECT max(%s) FROM %s), 0) + 1, false)", quoteIdent(column), table.qualified()), *targetSeq); err != nil {
				return fmt.Errorf("set sequence %s: %w", *targetSeq, err)
			}
		}
	}
	return nil
}
