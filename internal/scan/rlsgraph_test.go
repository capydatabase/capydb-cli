package scan

import "testing"

func TestTableRefsIgnoresCommentsAndUnknowns(t *testing.T) {
	known := map[string]bool{"messages": true, "conversations": true}
	body := `
	  -- select 1 from secrets
	  select 1 from messages m
	  join conversations c on c.id = m.conversation_id
	  where exists (select 1 from unrelated_table u where u.id = m.id)`
	got := tableRefsIn(body, known)
	if len(got) != 2 || got[0] != "conversations" || got[1] != "messages" {
		t.Fatalf("tableRefsIn = %v, want [conversations messages]", got)
	}
}

func TestWritesAnyDistinguishesReadFromWrite(t *testing.T) {
	known := map[string]bool{"notifications": true}
	if writesAny(`select * from notifications`, known) {
		t.Error("a read must not count as a write")
	}
	if !writesAny(`insert into notifications (id) values (1)`, known) {
		t.Error("insert into a known table must count as a write")
	}
	// A write to a table without RLS is not the bypass-loss class.
	if writesAny(`insert into audit_log (id) values (1)`, known) {
		t.Error("writes to unknown tables must not count")
	}
}

// The shape that fails statically: a policy on `conversations` calling a helper
// that reads `conversations`. Postgres refuses it outright under FORCE RLS.
func TestBuildPolicyCyclesFindsSameTableCycle(t *testing.T) {
	tables := map[string]bool{"conversations": true, "messages": true}
	routines := []routine{
		{Name: "users_in_conversation", Definer: true,
			Body: `select exists (select 1 from conversations c where c.id = cid)`},
		{Name: "unrelated", Body: `select now()`},
	}
	policies := []policyRef{
		{Table: "conversations", Name: "conv_read", Expr: `users_in_conversation(id)`},
		// Same helper from a DIFFERENT table is legitimate - that is why the
		// helper exists - and must not be reported.
		{Table: "messages", Name: "msg_read", Expr: `users_in_conversation(conversation_id)`},
		{Table: "messages", Name: "msg_other", Expr: `unrelated()`},
	}
	cycles := buildPolicyCycles(policies, routines, tables)
	if len(cycles) != 1 {
		t.Fatalf("want exactly one cycle, got %d: %+v", len(cycles), cycles)
	}
	c := cycles[0]
	if c.Table != "conversations" || c.Policy != "conv_read" || c.Helper != "users_in_conversation" || !c.SameTable {
		t.Fatalf("unexpected cycle %+v", c)
	}
}

// A helper that reaches the table only through a second helper still closes the
// loop; the walk has to follow calls, not just direct table references.
func TestBuildPolicyCyclesFollowsNestedCalls(t *testing.T) {
	tables := map[string]bool{"orgs": true}
	routines := []routine{
		{Name: "outer", Body: `select inner_helper()`},
		{Name: "inner_helper", Body: `select 1 from orgs`},
	}
	policies := []policyRef{{Table: "orgs", Name: "org_read", Expr: `outer()`}}
	if got := buildPolicyCycles(policies, routines, tables); len(got) != 1 {
		t.Fatalf("nested call cycle not found: %+v", got)
	}
}

// Mutual recursion between helpers must terminate rather than hang the scan.
func TestBuildPolicyCyclesTerminatesOnMutualRecursion(t *testing.T) {
	tables := map[string]bool{"t": true}
	routines := []routine{
		{Name: "a", Body: `select b()`},
		{Name: "b", Body: `select a()`},
	}
	policies := []policyRef{{Table: "t", Name: "p", Expr: `a()`}}
	if got := buildPolicyCycles(policies, routines, tables); len(got) != 0 {
		t.Fatalf("want no cycle (neither helper reaches t), got %+v", got)
	}
}

func TestInertPolicyRisk(t *testing.T) {
	e := SourceRLSExposure{Tables: 171, RLSEnabled: 171, RLSForced: 3}
	if got := e.InertPolicyRisk(); got != 168 {
		t.Fatalf("InertPolicyRisk = %d, want 168", got)
	}
	if got := (SourceRLSExposure{RLSEnabled: 5, RLSForced: 5}).InertPolicyRisk(); got != 0 {
		t.Fatalf("fully forced corpus must report 0 risk, got %d", got)
	}
}
