package scan

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"
)

// The three analyses in this file exist because `migrate scan` systematically
// UNDER-predicted the myroomiev3 migration: every one of them had to be written
// by hand mid-flight, and each one changed the plan. An under-predicting scan is
// worse than no scan, because the plan is made against it.
//
// All three answer the same question from different angles: what stops working
// when the app stops connecting as a role that bypasses RLS?

// SourceRLSExposure separates "RLS is enabled" from "RLS applies to the role the
// app will connect as". On a managed single-credential destination the app
// connects as the table owner, and an owner bypasses row security unless the
// table is FORCEd - so a corpus can be 100% RLS-enabled and 100% inert.
// myroomiev3 read as 171/171 enabled; FORCE was set on 3.
type SourceRLSExposure struct {
	Tables     int      `json:"tables"`
	RLSEnabled int      `json:"rls_enabled"`
	RLSForced  int      `json:"rls_forced"`
	Policies   int      `json:"policies"`
	Owners     []string `json:"owners"`
}

// InertPolicyRisk reports the tables whose policies would silently stop applying
// on a single-credential destination: RLS on, FORCE off. Silently is the
// operative word - no error, no log line, correct-looking rows belonging to
// someone else.
func (e SourceRLSExposure) InertPolicyRisk() int {
	if e.RLSEnabled <= e.RLSForced {
		return 0
	}
	return e.RLSEnabled - e.RLSForced
}

// SourceDefinerFunction is a SECURITY DEFINER function that reaches a table
// carrying RLS. On the source these run as the owner and bypass row security -
// that is usually WHY they are definer (cross-user writes: notify the other
// party, create both sides of a match, provision a profile). Under FORCE RLS
// with one owning role that bypass is gone and the body starts failing.
type SourceDefinerFunction struct {
	Name   string   `json:"name"`
	Tables []string `json:"tables"`
	// Writes is true when the body INSERTs/UPDATEs/DELETEs one of those tables:
	// a lost bypass on a write surfaces as a WITH CHECK violation, which is the
	// loud case. A lost bypass on a read just returns fewer rows.
	Writes bool `json:"writes"`
	// SwallowsErrors marks a body ending in EXCEPTION WHEN OTHERS - the silent
	// class. 29 of myroomiev3's did.
	SwallowsErrors bool `json:"swallows_errors"`
}

// SourcePolicyCycle is a policy that reaches its own table through a helper
// function. SameTable cycles are a STATIC error: Postgres refuses the policy
// outright ("infinite recursion detected in policy"). Longer cycles recurse at
// run time through opaque plpgsql and surface as stack depth exhaustion.
type SourcePolicyCycle struct {
	Table     string   `json:"table"`
	Policy    string   `json:"policy"`
	Helper    string   `json:"helper"`
	Through   []string `json:"through"`
	SameTable bool     `json:"same_table"`
}

var (
	// Table references in a function body. Deliberately loose: this is a
	// static approximation used to rank work, not a parser.
	tableRefPattern = regexp.MustCompile(`(?is)\b(?:from|join|update|into|delete\s+from)\s+(?:only\s+)?(?:public\.)?"?([a-z_][a-z0-9_]*)"?`)
	callPattern     = regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\s*\(`)
	writePattern    = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from)\s+(?:only\s+)?(?:public\.)?"?([a-z_][a-z0-9_]*)"?`)
	swallowPattern  = regexp.MustCompile(`(?is)exception\s+when\s+others`)
	sqlCommentLine  = regexp.MustCompile(`(?m)--.*$`)
)

// stripSQLComments removes line comments so a commented-out query does not
// count as a table reference.
func stripSQLComments(body string) string {
	return sqlCommentLine.ReplaceAllString(body, "")
}

// tableRefsIn returns the known tables a body reads or writes.
func tableRefsIn(body string, known map[string]bool) []string {
	body = stripSQLComments(body)
	seen := map[string]bool{}
	for _, match := range tableRefPattern.FindAllStringSubmatch(body, -1) {
		name := strings.ToLower(match[1])
		if known[name] {
			seen[name] = true
		}
	}
	return sortedKeys(seen)
}

// callsIn returns the known function names a body or expression calls.
func callsIn(text string, known map[string]bool) []string {
	text = stripSQLComments(text)
	seen := map[string]bool{}
	for _, match := range callPattern.FindAllStringSubmatch(text, -1) {
		name := strings.ToLower(match[1])
		if known[name] {
			seen[name] = true
		}
	}
	return sortedKeys(seen)
}

func writesAny(body string, known map[string]bool) bool {
	body = stripSQLComments(body)
	for _, match := range writePattern.FindAllStringSubmatch(body, -1) {
		if known[strings.ToLower(match[1])] {
			return true
		}
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// routine is one function body, as probed.
type routine struct {
	Name    string
	Body    string
	Definer bool
}

// policyRef is one policy expression, as probed.
type policyRef struct {
	Table string
	Name  string
	Expr  string
}

// buildPolicyCycles finds policies that reach their own table through helper
// functions. Pure, so it is testable without a database.
func buildPolicyCycles(policies []policyRef, routines []routine, tables map[string]bool) []SourcePolicyCycle {
	byName := make(map[string]routine, len(routines))
	known := make(map[string]bool, len(routines))
	for _, r := range routines {
		byName[r.Name] = r
		known[r.Name] = true
	}

	// reads(fn) = every table the function reaches, following calls it makes.
	memo := map[string][]string{}
	var reads func(name string, seen map[string]bool) []string
	reads = func(name string, seen map[string]bool) []string {
		if cached, ok := memo[name]; ok {
			return cached
		}
		if seen[name] {
			return nil
		}
		fn, ok := byName[name]
		if !ok {
			return nil
		}
		seen[name] = true
		defer delete(seen, name)

		out := map[string]bool{}
		for _, table := range tableRefsIn(fn.Body, tables) {
			out[table] = true
		}
		for _, callee := range callsIn(fn.Body, known) {
			if callee == name {
				continue
			}
			for _, table := range reads(callee, seen) {
				out[table] = true
			}
		}
		result := sortedKeys(out)
		memo[name] = result
		return result
	}

	var cycles []SourcePolicyCycle
	for _, policy := range policies {
		for _, helper := range callsIn(policy.Expr, known) {
			for _, reached := range reads(helper, map[string]bool{}) {
				if reached != policy.Table {
					continue
				}
				cycles = append(cycles, SourcePolicyCycle{
					Table:     policy.Table,
					Policy:    policy.Name,
					Helper:    helper,
					SameTable: true,
				})
			}
		}
	}
	sort.Slice(cycles, func(i, j int) bool {
		if cycles[i].Table != cycles[j].Table {
			return cycles[i].Table < cycles[j].Table
		}
		return cycles[i].Policy < cycles[j].Policy
	})
	return cycles
}

// probeRLSExposure answers "FORCE on X of Y, owned by whom".
func probeRLSExposure(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	err := conn.QueryRowContext(ctx, `
		select count(*),
		       count(*) filter (where c.relrowsecurity),
		       count(*) filter (where c.relforcerowsecurity)
		from pg_catalog.pg_class c
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace
		where c.relkind = 'r'
		  and n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')`).
		Scan(&facts.RLSExposure.Tables, &facts.RLSExposure.RLSEnabled, &facts.RLSExposure.RLSForced)
	if err != nil {
		return err
	}
	facts.RLSExposure.Policies = facts.Policies.Total

	rows, err := conn.QueryContext(ctx, `
		select distinct pg_catalog.pg_get_userbyid(c.relowner)
		from pg_catalog.pg_class c
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace
		where c.relkind = 'r'
		  and n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
		order by 1`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return err
		}
		facts.RLSExposure.Owners = append(facts.RLSExposure.Owners, owner)
	}
	return rows.Err()
}

// probeRLSGraph loads app tables, policy expressions and function bodies once,
// then derives the definer classification and the cycle graph from them.
func probeRLSGraph(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	rlsTables := map[string]bool{}
	rows, err := conn.QueryContext(ctx, `
		select c.relname
		from pg_catalog.pg_class c
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace
		where c.relkind = 'r' and c.relrowsecurity
		  and n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		rlsTables[strings.ToLower(name)] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var routines []routine
	rows, err = conn.QueryContext(ctx, `
		select p.proname, p.prosrc, p.prosecdef
		from pg_catalog.pg_proc p
		join pg_catalog.pg_namespace n on n.oid = p.pronamespace
		where n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
		  and p.prokind = 'f'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r routine
		if err := rows.Scan(&r.Name, &r.Body, &r.Definer); err != nil {
			_ = rows.Close()
			return err
		}
		r.Name = strings.ToLower(r.Name)
		routines = append(routines, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var policies []policyRef
	rows, err = conn.QueryContext(ctx, `
		select tablename, policyname, coalesce(qual, '') || ' ' || coalesce(with_check, '')
		from pg_catalog.pg_policies
		where schemaname not in (`+quotedSchemaList()+`)`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p policyRef
		if err := rows.Scan(&p.Table, &p.Name, &p.Expr); err != nil {
			return err
		}
		p.Table = strings.ToLower(p.Table)
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range routines {
		if !r.Definer {
			continue
		}
		reached := tableRefsIn(r.Body, rlsTables)
		if len(reached) == 0 {
			continue
		}
		facts.DefinerFunctions = append(facts.DefinerFunctions, SourceDefinerFunction{
			Name:           r.Name,
			Tables:         reached,
			Writes:         writesAny(r.Body, rlsTables),
			SwallowsErrors: swallowPattern.MatchString(r.Body),
		})
	}
	sort.Slice(facts.DefinerFunctions, func(i, j int) bool {
		return facts.DefinerFunctions[i].Name < facts.DefinerFunctions[j].Name
	})

	facts.PolicyCycles = buildPolicyCycles(policies, routines, rlsTables)
	return nil
}
