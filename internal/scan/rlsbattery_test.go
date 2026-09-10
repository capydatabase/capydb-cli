package scan

import "testing"

func cells(items ...BatteryCell) []BatteryCell { return items }

func TestDiffBatteryIdentical(t *testing.T) {
	side := cells(
		BatteryCell{Context: "anon", Table: "posts", Count: 0},
		BatteryCell{Context: "user_a", Table: "posts", Count: 3},
	)
	report := DiffBattery(side, side)
	if !report.Equivalent() {
		t.Fatalf("identical runs must be equivalent: %+v", report)
	}
	if report.Checks != 2 || report.Contexts != 2 || report.Tables != 1 {
		t.Fatalf("unexpected shape: %+v", report)
	}
}

// The dangerous direction: the new database shows MORE rows to an identity
// than the old one did.
func TestDiffBatteryCatchesWidenedVisibility(t *testing.T) {
	source := cells(BatteryCell{Context: "user_a", Table: "messages", Count: 2})
	target := cells(BatteryCell{Context: "user_a", Table: "messages", Count: 40})
	report := DiffBattery(source, target)
	if report.Equivalent() {
		t.Fatal("a widened count must not be reported as equivalent")
	}
	d := report.Divergences[0]
	if d.Source != "2" || d.Target != "40" {
		t.Fatalf("unexpected divergence %+v", d)
	}
}

// A denial on both sides is equivalence - policies that deny identically have
// migrated correctly, and treating that as a failure would make the tool
// useless on any corpus with restrictive policies.
func TestDiffBatteryTreatsMatchingDenialAsEquivalent(t *testing.T) {
	side := cells(BatteryCell{Context: "anon", Table: "secrets", Error: "42501"})
	if !DiffBattery(side, side).Equivalent() {
		t.Fatal("matching denials must count as equivalent")
	}
}

// Denied on the old side, readable on the new one, is the silent-exposure case
// this whole command exists to catch.
func TestDiffBatteryCatchesDenialBecomingReadable(t *testing.T) {
	source := cells(BatteryCell{Context: "anon", Table: "secrets", Error: "42501"})
	target := cells(BatteryCell{Context: "anon", Table: "secrets", Count: 100})
	report := DiffBattery(source, target)
	if report.Equivalent() {
		t.Fatal("denied -> readable must be reported")
	}
	if report.Divergences[0].Source != "ERR 42501" || report.Divergences[0].Target != "100" {
		t.Fatalf("unexpected divergence %+v", report.Divergences[0])
	}
}

func TestDiffBatteryReportsMissingTables(t *testing.T) {
	source := cells(BatteryCell{Context: "anon", Table: "gone", Count: 1})
	target := cells(BatteryCell{Context: "anon", Table: "added", Count: 1})
	report := DiffBattery(source, target)
	if len(report.SourceOnly) != 1 || report.SourceOnly[0] != "gone" {
		t.Fatalf("source-only wrong: %+v", report.SourceOnly)
	}
	if len(report.TargetOnly) != 1 || report.TargetOnly[0] != "added" {
		t.Fatalf("target-only wrong: %+v", report.TargetOnly)
	}
	if report.Equivalent() {
		t.Fatal("a table present on only one side is not equivalence")
	}
}

func TestParseBatteryContextsRejectsUnlabelled(t *testing.T) {
	if _, err := ParseBatteryContexts([]byte(`[{"claims":{}}]`)); err == nil {
		t.Fatal("a context without a label must be rejected")
	}
	if _, err := ParseBatteryContexts([]byte(`[]`)); err == nil {
		t.Fatal("an empty contexts file must be rejected")
	}
	got, err := ParseBatteryContexts([]byte(`[{"label":"anon","claims":{}}]`))
	if err != nil || len(got) != 1 || got[0].Label != "anon" {
		t.Fatalf("valid contexts rejected: %v %+v", err, got)
	}
}

// A read and a write probe on the same table+context are different cells. If
// Mode were not part of the key they would collide and the write result would
// silently overwrite the read one.
func TestDiffBatteryKeysReadAndWriteSeparately(t *testing.T) {
	source := cells(
		BatteryCell{Context: "user_a", Table: "posts", Mode: "read", Count: 2},
		BatteryCell{Context: "user_a", Table: "posts", Mode: "write", Count: 1},
	)
	// Same reads, wider writes - the drift a read-only battery cannot see.
	target := cells(
		BatteryCell{Context: "user_a", Table: "posts", Mode: "read", Count: 2},
		BatteryCell{Context: "user_a", Table: "posts", Mode: "write", Count: 2},
	)
	report := DiffBattery(source, target)
	if report.Checks != 2 {
		t.Fatalf("read and write must be separate checks, got %d", report.Checks)
	}
	if len(report.Divergences) != 1 {
		t.Fatalf("want exactly the write divergence, got %+v", report.Divergences)
	}
	d := report.Divergences[0]
	if d.Mode != "write" || d.Source != "1" || d.Target != "2" {
		t.Fatalf("unexpected divergence %+v", d)
	}
}
