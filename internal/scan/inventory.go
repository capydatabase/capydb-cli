package scan

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

// Physical inventory of the source database: what has to be copied, and which
// shapes inside it make the copy slower, riskier, or impossible to stream.
//
// The repo scan (scan.go) says what the application believes; the coupling
// probes (source.go) say how the application is bound to its provider; this
// file says what the bytes look like. All three are needed to size a migration:
// a 40-table app with one 800 GB append-only table and a primary-key-less audit
// table is a completely different job from a 400-table app of even size, and
// nothing in the repo distinguishes them.
//
// Every query here reads catalog and statistics views only - no table is
// touched, nothing is counted with count(*), and reltuples/pg_stat_* estimates
// are used deliberately: an exact count on a large table would blow the
// session's statement timeout and buy nothing a plan depends on.

// Inventory caps. The report is read by a person and rendered on a page, so the
// long tails are summarized by count rather than listed in full.
const (
	maxListedTables    = 25
	maxListedIndexes   = 25
	maxListedSequences = 25
	maxListedCycles    = 10

	// largeTableBytes is where a table stops being ordinary and starts
	// dictating the migration window on its own.
	largeTableBytes = 100 << 30 // 100 GiB
	// veryLargeTableBytes is where a single table needs its own plan.
	veryLargeTableBytes = 500 << 30 // 500 GiB
	// sequenceExhaustionRatio is the fraction of a sequence's range past which
	// it is worth widening the column before, not after, the move.
	sequenceExhaustionRatio = 0.8
)

// SourceInventory is the physical shape of the source database.
type SourceInventory struct {
	TableCount        int   `json:"table_count"`
	PartitionedTables int   `json:"partitioned_tables"`
	TotalTableBytes   int64 `json:"total_table_bytes"`
	IndexBytes        int64 `json:"index_bytes"`
	ToastBytes        int64 `json:"toast_bytes"`

	// Tables holds the largest tables, capped at maxListedTables. Partitions
	// are folded into their parent: a natively partitioned table migrates as
	// one object and its parts are not separately interesting.
	Tables []SourceTable `json:"tables"`

	// LargeTables and VeryLargeTables count every table over the thresholds,
	// including any beyond the listing cap.
	LargeTables     int `json:"large_tables"`
	VeryLargeTables int `json:"very_large_tables"`

	// TablesWithoutPrimaryKey cannot be streamed row-by-row unless they are set
	// to REPLICA IDENTITY FULL, which replicates the whole old row per change.
	TablesWithoutPrimaryKey []string `json:"tables_without_primary_key"`
	// TablesWithoutReplicaIdentity is the subset of the above that has no
	// replica identity either - the set that silently breaks a streaming
	// import at the first UPDATE.
	TablesWithoutReplicaIdentity []string `json:"tables_without_replica_identity"`

	EmptyTables int `json:"empty_tables"`

	UnusedIndexes     []SourceIndex `json:"unused_indexes"`
	UnusedIndexBytes  int64         `json:"unused_index_bytes"`
	DuplicateIndexes  []SourceIndex `json:"duplicate_indexes"`
	DuplicateIdxBytes int64         `json:"duplicate_index_bytes"`

	// ForeignKeyCycles are groups of tables that reference each other, so no
	// load order satisfies every constraint and the restore has to defer them.
	ForeignKeyCycles [][]string `json:"foreign_key_cycles"`

	// SequencesNearExhaustion have consumed most of their range. Widening the
	// column is far cheaper before the move than after it.
	SequencesNearExhaustion []SourceSequence `json:"sequences_near_exhaustion"`
}

// SourceTable is one user table, sized.
type SourceTable struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// Bytes is the total relation size: heap, indexes, and TOAST.
	Bytes int64 `json:"bytes"`
	// Rows is the planner's estimate, never an exact count. -1 means the table
	// has never been analysed, so the row count is unknown - which is not the
	// same as zero.
	Rows        int64 `json:"rows"`
	Partitioned bool  `json:"partitioned"`
}

// SourceIndex is one index the migration would copy for no benefit.
type SourceIndex struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	// DuplicateOf names the index covering the same columns, for duplicates.
	DuplicateOf string `json:"duplicate_of,omitempty"`
}

// SourceSequence is a sequence close to the end of its range.
type SourceSequence struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// UsedRatio is last_value/max_value, 0..1.
	UsedRatio float64 `json:"used_ratio"`
	DataType  string  `json:"data_type"`
}

// probeInventory fills facts.Inventory. Each sub-probe is independent so a
// permission failure on one view (pg_stat_user_indexes on a locked-down managed
// source, say) costs that section only.
func probeInventory(ctx context.Context, conn *sql.Conn, facts *SourceFacts, note func(string, error)) {
	inventory := &facts.Inventory
	if err := probeTables(ctx, conn, inventory); err != nil {
		note("table inventory", err)
	}
	if err := probeReplicaIdentity(ctx, conn, inventory); err != nil {
		note("replica identity", err)
	}
	if err := probeIndexes(ctx, conn, inventory); err != nil {
		note("index inventory", err)
	}
	if err := probeForeignKeyCycles(ctx, conn, inventory); err != nil {
		note("foreign key cycles", err)
	}
	if err := probeSequences(ctx, conn, inventory); err != nil {
		note("sequence headroom", err)
	}
}

// probeTables sizes every ordinary and partitioned table outside the system and
// provider-managed schemas. Partitions are excluded from the listing and summed
// into their root parent: a natively partitioned table migrates as one object,
// and its parts are not separately interesting.
func probeTables(ctx context.Context, conn *sql.Conn, inventory *SourceInventory) error {
	// pg_total_relation_size on a partitioned ROOT reports the root's own
	// (empty) storage, never the partitions'. Without pg_partition_tree a
	// natively partitioned table sizes at zero, so a database that is mostly
	// one partitioned table reports as tiny - verified against PG17.
	rows, err := conn.QueryContext(ctx, `
		with roots as (
			select c.oid, n.nspname, c.relname, c.relkind, c.reltuples, c.reltoastrelid
			from pg_catalog.pg_class c
			join pg_catalog.pg_namespace n on n.oid = c.relnamespace
			where c.relkind in ('r', 'p')
			  and not c.relispartition
			  and n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
			  and n.nspname not like 'pg\_%'
		)
		select r.nspname,
		       r.relname,
		       case when r.relkind = 'p'
		            then (select coalesce(sum(pg_total_relation_size(p.relid)), 0)
		                  from pg_catalog.pg_partition_tree(r.oid) p)
		            else pg_total_relation_size(r.oid) end,
		       case when r.relkind = 'p'
		            then (select coalesce(sum(pg_indexes_size(p.relid)), 0)
		                  from pg_catalog.pg_partition_tree(r.oid) p)
		            else pg_indexes_size(r.oid) end,
		       coalesce(pg_total_relation_size(r.reltoastrelid), 0),
		       case when r.relkind = 'p'
		            then (select coalesce(sum(nullif(c.reltuples, -1)), -1)
		                  from pg_catalog.pg_partition_tree(r.oid) p
		                  join pg_catalog.pg_class c on c.oid = p.relid
		                  where p.isleaf)
		            else r.reltuples end::bigint,
		       r.relkind = 'p',
		       exists (
		           select 1 from pg_catalog.pg_index i
		           where i.indrelid = r.oid and i.indisprimary
		       )
		from roots r
		order by 3 desc`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var table SourceTable
		var indexBytes, toastBytes int64
		var hasPrimaryKey bool
		if err := rows.Scan(&table.Schema, &table.Name, &table.Bytes, &indexBytes, &toastBytes,
			&table.Rows, &table.Partitioned, &hasPrimaryKey); err != nil {
			return err
		}

		inventory.TableCount++
		inventory.TotalTableBytes += table.Bytes
		inventory.IndexBytes += indexBytes
		inventory.ToastBytes += toastBytes
		if table.Partitioned {
			inventory.PartitionedTables++
		}
		switch {
		case table.Bytes > veryLargeTableBytes:
			inventory.VeryLargeTables++
			inventory.LargeTables++
		case table.Bytes > largeTableBytes:
			inventory.LargeTables++
		}
		// reltuples is -1 on a table that has never been ANALYZEd (PG14+), which
		// is not the same as empty. Counting it as empty told the first run
		// against a fresh restore that every table was abandoned.
		if table.Rows == 0 {
			inventory.EmptyTables++
		}
		if !hasPrimaryKey {
			inventory.TablesWithoutPrimaryKey = append(inventory.TablesWithoutPrimaryKey,
				table.Schema+"."+table.Name)
		}
		if len(inventory.Tables) < maxListedTables {
			inventory.Tables = append(inventory.Tables, table)
		}
	}
	return rows.Err()
}

// probeReplicaIdentity narrows the primary-key-less set to the tables a
// streaming import would actually break on. A table with REPLICA IDENTITY FULL
// or a nominated unique index replicates correctly (more expensively); one left
// on the default with no primary key does not - the publisher rejects the
// UPDATE, mid-migration, with no warning at setup time.
func probeReplicaIdentity(ctx context.Context, conn *sql.Conn, inventory *SourceInventory) error {
	rows, err := conn.QueryContext(ctx, `
		select n.nspname || '.' || c.relname
		from pg_catalog.pg_class c
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace
		where c.relkind = 'r'
		  and not c.relispartition
		  and c.relreplident = 'd'
		  and n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
		  and n.nspname not like 'pg\_%'
		  and not exists (
		      select 1 from pg_catalog.pg_index i
		      where i.indrelid = c.oid and i.indisprimary
		  )
		order by 1`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return err
		}
		inventory.TablesWithoutReplicaIdentity = append(inventory.TablesWithoutReplicaIdentity, table)
	}
	return rows.Err()
}

// probeIndexes finds indexes worth dropping before the move: never-scanned
// ones, and exact duplicates. Both inflate the copy twice over - the bytes
// cross the wire, and every index is rebuilt on the target.
//
// idx_scan is cumulative since the last stats reset, so a freshly reset source
// reports everything as unused. That is why the finding is advisory copy
// ("review"), never a blocker.
func probeIndexes(ctx context.Context, conn *sql.Conn, inventory *SourceInventory) error {
	// An index can be both never-scanned and a duplicate of another. Dropping it
	// reclaims one index's worth of bytes either way, so the duplicate pass
	// skips whatever the unused pass already counted - otherwise
	// ReclaimableBytes promises a saving that does not exist.
	unused := map[string]bool{}
	rows, err := conn.QueryContext(ctx, `
		select s.schemaname, s.relname, s.indexrelname, pg_relation_size(s.indexrelid)
		from pg_catalog.pg_stat_user_indexes s
		join pg_catalog.pg_index i on i.indexrelid = s.indexrelid
		where s.idx_scan = 0
		  and not i.indisprimary
		  and not i.indisunique
		  and s.schemaname not in (`+quotedSchemaList()+`)
		order by pg_relation_size(s.indexrelid) desc`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var index SourceIndex
		if err := rows.Scan(&index.Schema, &index.Table, &index.Name, &index.Bytes); err != nil {
			_ = rows.Close()
			return err
		}
		inventory.UnusedIndexBytes += index.Bytes
		unused[index.Schema+"."+index.Name] = true
		if len(inventory.UnusedIndexes) < maxListedIndexes {
			inventory.UnusedIndexes = append(inventory.UnusedIndexes, index)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Duplicates: same table, same indexed columns and expression set. Compared
	// on indkey plus the normalized definition so a partial index and a full
	// index over the same columns are not called duplicates.
	rows, err = conn.QueryContext(ctx, `
		select n.nspname,
		       t.relname,
		       i.relname,
		       pg_relation_size(i.oid),
		       ix.indkey::text || ' ' || coalesce(pg_get_expr(ix.indpred, ix.indrelid), '')
		from pg_catalog.pg_index ix
		join pg_catalog.pg_class i on i.oid = ix.indexrelid
		join pg_catalog.pg_class t on t.oid = ix.indrelid
		join pg_catalog.pg_namespace n on n.oid = t.relnamespace
		where n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
		  and n.nspname not like 'pg\_%'
		order by n.nspname, t.relname, pg_relation_size(i.oid) desc`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	type indexKey struct{ schema, table, signature string }
	seen := map[indexKey]string{}
	for rows.Next() {
		var index SourceIndex
		var signature string
		if err := rows.Scan(&index.Schema, &index.Table, &index.Name, &index.Bytes, &signature); err != nil {
			return err
		}
		key := indexKey{index.Schema, index.Table, signature}
		if first, ok := seen[key]; ok {
			index.DuplicateOf = first
			if !unused[index.Schema+"."+index.Name] {
				inventory.DuplicateIdxBytes += index.Bytes
			}
			if len(inventory.DuplicateIndexes) < maxListedIndexes {
				inventory.DuplicateIndexes = append(inventory.DuplicateIndexes, index)
			}
			continue
		}
		seen[key] = index.Name
	}
	return rows.Err()
}

// probeForeignKeyCycles reports groups of tables that reference each other.
// A cycle has no valid insertion order, so the restore must defer the
// constraints - and any migration that copies table-by-table with constraints
// live will deadlock or fail on it.
//
// Self-references are excluded: a single table pointing at itself is ordinary
// (a tree), and pg_restore handles it without deferral.
func probeForeignKeyCycles(ctx context.Context, conn *sql.Conn, inventory *SourceInventory) error {
	rows, err := conn.QueryContext(ctx, `
		select source_ns.nspname || '.' || source_tbl.relname,
		       target_ns.nspname || '.' || target_tbl.relname
		from pg_catalog.pg_constraint con
		join pg_catalog.pg_class source_tbl on source_tbl.oid = con.conrelid
		join pg_catalog.pg_namespace source_ns on source_ns.oid = source_tbl.relnamespace
		join pg_catalog.pg_class target_tbl on target_tbl.oid = con.confrelid
		join pg_catalog.pg_namespace target_ns on target_ns.oid = target_tbl.relnamespace
		where con.contype = 'f'
		  and con.conrelid <> con.confrelid
		  and source_ns.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
		  and target_ns.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	edges := map[string][]string{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return err
		}
		edges[from] = append(edges[from], to)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	inventory.ForeignKeyCycles = stronglyConnectedGroups(edges)
	return nil
}

// stronglyConnectedGroups returns every strongly connected component with more
// than one member - i.e. every set of tables that can reach each other through
// foreign keys. Iterative Tarjan: a deeply linked schema would blow the stack
// on the recursive form.
func stronglyConnectedGroups(edges map[string][]string) [][]string {
	nodes := make([]string, 0, len(edges))
	for node := range edges {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	index := map[string]int{}
	lowlink := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	counter := 0
	groups := [][]string{}

	// frame is one node's position in the iterative depth-first search: which
	// of its out-edges has already been walked.
	type frame struct {
		node string
		edge int
	}

	for _, root := range nodes {
		if _, visited := index[root]; visited {
			continue
		}
		work := []frame{{node: root}}
		index[root], lowlink[root] = counter, counter
		counter++
		stack = append(stack, root)
		onStack[root] = true

		for len(work) > 0 {
			current := &work[len(work)-1]
			if current.edge < len(edges[current.node]) {
				next := edges[current.node][current.edge]
				current.edge++
				if _, visited := index[next]; !visited {
					index[next], lowlink[next] = counter, counter
					counter++
					stack = append(stack, next)
					onStack[next] = true
					work = append(work, frame{node: next})
				} else if onStack[next] {
					lowlink[current.node] = min(lowlink[current.node], index[next])
				}
				continue
			}

			// Every out-edge walked: close the node out.
			if lowlink[current.node] == index[current.node] {
				var group []string
				for {
					top := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[top] = false
					group = append(group, top)
					if top == current.node {
						break
					}
				}
				if len(group) > 1 {
					sort.Strings(group)
					groups = append(groups, group)
				}
			}
			done := current.node
			work = work[:len(work)-1]
			if len(work) > 0 {
				parent := work[len(work)-1].node
				lowlink[parent] = min(lowlink[parent], lowlink[done])
			}
		}
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	if len(groups) > maxListedCycles {
		groups = groups[:maxListedCycles]
	}
	return groups
}

// probeSequences reports sequences that have consumed most of their range.
// The classic case is a serial (int4) primary key past two billion: the column
// has to widen to bigint, which is a rewrite - far cheaper to do on the source
// before the move than on a freshly imported database under load.
func probeSequences(ctx context.Context, conn *sql.Conn, inventory *SourceInventory) error {
	rows, err := conn.QueryContext(ctx, `
		select schemaname,
		       sequencename,
		       data_type::text,
		       last_value::numeric / nullif(max_value, 0)::numeric
		from pg_catalog.pg_sequences
		where last_value is not null
		  and max_value > 0
		  and last_value::numeric / max_value::numeric >= $1
		  and schemaname not in (`+quotedSchemaList()+`)
		order by 4 desc`, sequenceExhaustionRatio)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sequence SourceSequence
		if err := rows.Scan(&sequence.Schema, &sequence.Name, &sequence.DataType, &sequence.UsedRatio); err != nil {
			return err
		}
		if len(inventory.SequencesNearExhaustion) < maxListedSequences {
			inventory.SequencesNearExhaustion = append(inventory.SequencesNearExhaustion, sequence)
		}
	}
	return rows.Err()
}

// TotalBytes is the whole copy: every user table with its indexes and TOAST.
func (i SourceInventory) TotalBytes() int64 { return i.TotalTableBytes }

// ReclaimableBytes is what dropping the unused and duplicated indexes would
// take off the wire.
func (i SourceInventory) ReclaimableBytes() int64 {
	return i.UnusedIndexBytes + i.DuplicateIdxBytes
}

// CycleSummary renders the FK cycles as one line each.
func (i SourceInventory) CycleSummary() []string {
	summary := make([]string, 0, len(i.ForeignKeyCycles))
	for _, cycle := range i.ForeignKeyCycles {
		summary = append(summary, strings.Join(cycle, " <-> "))
	}
	return summary
}
