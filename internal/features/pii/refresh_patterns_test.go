package pii

import (
	"strings"
	"testing"
)

// TestRefreshEnabledPatterns verifies a pattern created at runtime (after
// the masker was constructed) becomes active immediately after
// RefreshEnabledPatterns, without a restart — the backend live-apply path
// behind the dashboard pattern CRUD.
func TestRefreshEnabledPatterns(t *testing.T) {
	// 1. Build a masker from the current snapshot (default: everything loaded).
	base := DefaultPIIPatterns
	LoadPatterns(base)
	m := NewMasker("mask", nil)

	// A pattern that is NOT in the initial snapshot and does not collide with
	// any default (defaults match emails, digits, phone-like strings — use
	// a letters-heavy token so nothing else fires).
	newPattern := Pattern{Name: "customer_token", Regex: `CUST-[A-Z]{4}[0-9]{2}`, Enabled: true, Action: ActionMask}
	if _, ok := LoadedPatterns[newPattern.Name]; ok {
		t.Fatalf("test pattern %q must not collide with defaults", newPattern.Name)
	}

	// 2. Before refresh: the new pattern name is absent from the masker's
	// snapshot, so it must NOT be detected.
	if !strings.Contains(maskedText(m), "CUST-ABCD12") {
		t.Fatal("precondition failed: CUST-ABCD12 should be untouched before refresh")
	}

	// 3. Simulate dashboard create: load DB state (defaults + new pattern).
	LoadPatterns(append(base, newPattern))

	// 4. Before refresh the new name still isn't applied (snapshot is stale).
	if !strings.Contains(maskedText(m), "CUST-ABCD12") {
		t.Fatal("expected stale snapshot to not yet mask the new pattern")
	}

	// 5. Refresh → the new pattern is now active.
	m.RefreshEnabledPatterns()
	out := maskedText(m)
	if strings.Contains(out, "CUST-ABCD12") {
		t.Fatalf("after refresh, CUST-ABCD12 must be masked; got %q", out)
	}

	// 6. Disabled patterns are skipped after refresh.
	LoadPatterns(append(base, Pattern{Name: "customer_token", Regex: `CUST-[A-Z]{4}[0-9]{2}`, Enabled: false, Action: ActionMask}))
	m.RefreshEnabledPatterns()
	if !strings.Contains(maskedText(m), "CUST-ABCD12") {
		t.Fatal("disabled pattern must not mask after refresh")
	}

	// 7. Pinned-pattern maskers ignore refresh (operator-configured set stays).
	LoadPatterns(append(base, newPattern))
	pinned := NewMasker("mask", []string{"email"})
	pinned.RefreshEnabledPatterns()
	if !strings.Contains(maskedText(pinned), "CUST-ABCD12") {
		t.Fatal("pinned masker must not pick up new patterns via refresh")
	}
}

// maskedText runs the masker and returns the masked result (unmasking the
// placeholder back to the match isn't needed — we assert on the raw value
// disappearing or the placeholder appearing).
func maskedText(m *Masker) string {
	out, err := m.ProcessText("ref CUST-ABCD12", nil)
	if err != nil {
		// block action would error; tests here only use mask
		panic(err)
	}
	return out
}
