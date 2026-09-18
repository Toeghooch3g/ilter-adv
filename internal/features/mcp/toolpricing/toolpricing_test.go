package toolpricing

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeReader implements RuntimeConfigReader over a static map.
type fakeReader map[string]string

func (f fakeReader) GetBySection(_ context.Context, section string) (map[string]string, error) {
	if section != "tool_pricing" {
		return nil, nil
	}
	return f, nil
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestLoadParsesRules(t *testing.T) {
	rc := fakeReader{
		"kagi__search":  `{"server":"kagi","tool":"search","unit":"call","cost_per_unit":0.012}`,
		"kagi__extract": `{"server":"kagi","tool":"extract","unit":"page","cost_per_unit":0.004,"unit_source":"urls","unit_cap":10}`,
		"bad":           `{not json`,
	}
	rules, err := Load(context.Background(), rc)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 parseable rules (bad row skipped), got %d", len(rules))
	}
}

// TestCostForRuleMatching covers exact server + exact tool, glob server with
// exact tool, and no-rule → 0.
func TestCostForRuleMatching(t *testing.T) {
	res := NewResolver([]Rule{
		{Key: "kagi__search", Server: "kagi", Tool: "search", Unit: UnitCall, CostPerUnit: 0.012},
		{Key: "any__glob", Server: "*", Tool: "get_*", Unit: UnitCall, CostPerUnit: 0.001},
	})

	if got := res.CostFor("kagi", "search", nil); got != 0.012 {
		t.Errorf("exact server+tool: want 0.012, got %v", got)
	}
	if got := res.CostFor("other", "search", nil); got != 0 {
		t.Errorf("different server must not match exact rule, got %v", got)
	}
	if got := res.CostFor("kagi", "get_info", nil); got != 0.001 {
		t.Errorf("glob tool must match, got %v", got)
	}
	if got := res.CostFor("kagi", "unlisted", nil); got != 0 {
		t.Errorf("no matching rule must cost 0, got %v", got)
	}
}

// TestCostForUnitCounting covers per-page/url unit counting from the
// unit_source argument, unit_cap, and missing-argument defaults.
func TestCostForUnitCounting(t *testing.T) {
	res := NewResolver([]Rule{
		{Key: "kagi__extract", Server: "kagi", Tool: "extract", Unit: UnitPage, CostPerUnit: 0.004, UnitSource: "urls", UnitCap: 10},
	})

	// 3 URLs → 3 * 0.004 = 0.012.
	args := mustJSON(t, map[string]any{"urls": []any{"u1", "u2", "u3"}})
	if got := res.CostFor("kagi", "extract", args); got != 0.012 {
		t.Errorf("3 urls: want 0.012, got %v", got)
	}

	// 15 URLs capped at 10 → 0.04.
	urls := make([]any, 15)
	for i := range urls {
		urls[i] = "u"
	}
	args = mustJSON(t, map[string]any{"urls": urls})
	if got := res.CostFor("kagi", "extract", args); got != 0.04 {
		t.Errorf("15 urls capped at 10: want 0.04, got %v", got)
	}

	// Missing unit_source arg → single unit.
	if got := res.CostFor("kagi", "extract", nil); got != 0.004 {
		t.Errorf("no args: want 0.004, got %v", got)
	}
	// Empty urls array → 0 units → 0 cost.
	args = mustJSON(t, map[string]any{"urls": []any{}})
	if got := res.CostFor("kagi", "extract", args); got != 0 {
		t.Errorf("empty urls: want 0, got %v", got)
	}
}

// TestCostForSpecificity verifies the winning rule is the most specific one:
// exact server+exact tool beats glob server+exact tool beats any+glob.
func TestCostForSpecificity(t *testing.T) {
	res := NewResolver([]Rule{
		{Key: "broad", Server: "*", Tool: "*", Unit: UnitCall, CostPerUnit: 0.5},
		{Key: "server-glob", Server: "kag*", Tool: "search", Unit: UnitCall, CostPerUnit: 0.05},
		{Key: "exact", Server: "kagi", Tool: "search", Unit: UnitCall, CostPerUnit: 0.005},
	})

	if got := res.CostFor("kagi", "search", nil); got != 0.005 {
		t.Errorf("exact rule must win, got %v", got)
	}
	// Glob server + exact tool wins over the any-server catch-all.
	if got := res.CostFor("kagis", "search", nil); got != 0.05 {
		t.Errorf("glob-server rule must beat catch-all, got %v", got)
	}
	// Catch-all for anything unmatched.
	if got := res.CostFor("other", "misc", nil); got != 0.5 {
		t.Errorf("catch-all must apply, got %v", got)
	}
}

func TestRuleValidate(t *testing.T) {
	if err := (&Rule{Tool: "search", Unit: UnitCall, CostPerUnit: 0.012}).Validate(); err != nil {
		t.Errorf("valid rule rejected: %v", err)
	}
	if err := (&Rule{Tool: "", Unit: UnitCall}).Validate(); err == nil {
		t.Error("empty tool must be rejected")
	}
	if err := (&Rule{Tool: "search", Unit: "per-gigabyte"}).Validate(); err == nil {
		t.Error("invalid unit must be rejected")
	}
	if err := (&Rule{Tool: "search", Unit: UnitCall, CostPerUnit: -1}).Validate(); err == nil {
		t.Error("negative cost must be rejected")
	}
}
