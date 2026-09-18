// Package toolpricing implements per-tool MCP call pricing rules loaded from
// the runtime_config "tool_pricing" section (hot, dashboard-editable). Each
// rule bills a tool call at a fixed per-unit cost, where the unit is either
// the whole call ("call") or a count derived from an argument ("page"/"url",
// e.g. the number of URLs in a kagi extract call).
package toolpricing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
)

// RuntimeConfigReader abstracts read access over the runtime_config table.
// Defined locally (not config.RuntimeConfigReader) so this package never
// imports internal/config — config's validate.go imports this package for
// Rule.Validate, which would otherwise form an import cycle.
type RuntimeConfigReader interface {
	GetBySection(ctx context.Context, section string) (map[string]string, error)
}

// Unit is the billable unit of a pricing rule.
type Unit string

const (
	// UnitCall bills one fixed unit per tool call.
	UnitCall Unit = "call"
	// UnitPage/UnitURL bill per counted unit in the call's arguments
	// (e.g. pages processed, URLs extracted).
	UnitPage Unit = "page"
	UnitURL  Unit = "url"
)

// Rule is a single pricing rule. It lives in the runtime_config table under
// section "tool_pricing", keyed by an arbitrary rule name (e.g.
// "kagi__search"); the key itself is not part of the stored document.
type Rule struct {
	// Key is the runtime_config rule name (filled by Load; not serialized).
	Key string `json:"-"`

	// Server is the registry server ID to match, or "*" for any server.
	// A glob pattern is allowed.
	Server string `json:"server"`
	// Tool is the bare tool name or a glob pattern.
	Tool string `json:"tool"`
	// Unit is "call", "page", or "url".
	Unit Unit `json:"unit"`
	// CostPerUnit is the USD cost per unit (e.g. 0.012 for $12/1k calls).
	CostPerUnit float64 `json:"cost_per_unit"`
	// UnitSource names the argument key whose array length (or numeric
	// value) is the number of units when Unit is "page"/"url". Empty means
	// a single unit.
	UnitSource string `json:"unit_source,omitempty"`
	// UnitCap caps the units billable per call (0 = unlimited).
	UnitCap int `json:"unit_cap,omitempty"`
}

// Validate checks a single rule document. It is used by the dashboard write
// path (config.ValidateRuntimeConfig) via Rule.Validate.
func (r *Rule) Validate() error {
	if strings.TrimSpace(r.Tool) == "" {
		return fmt.Errorf("tool is required")
	}
	switch r.Unit {
	case UnitCall, UnitPage, UnitURL:
	default:
		return fmt.Errorf("invalid unit %q: must be one of call, page, url", r.Unit)
	}
	if r.CostPerUnit < 0 {
		return fmt.Errorf("cost_per_unit must not be negative")
	}
	if r.UnitCap < 0 {
		return fmt.Errorf("unit_cap must not be negative")
	}
	return nil
}

// Load reads every rule in the "tool_pricing" runtime_config section.
// Unparseable rows are logged and skipped so one bad rule never prevents the
// rest from loading.
func Load(ctx context.Context, rc RuntimeConfigReader) ([]Rule, error) {
	entries, err := rc.GetBySection(ctx, "tool_pricing")
	if err != nil {
		return nil, err
	}
	rules := make([]Rule, 0, len(entries))
	for key, raw := range entries {
		var r Rule
		if uErr := json.Unmarshal([]byte(raw), &r); uErr != nil {
			slog.Warn("toolpricing: skipping unparseable rule", "key", key, "error", uErr)
			continue
		}
		r.Key = key
		rules = append(rules, r)
	}
	return rules, nil
}

// Resolver matches tool calls against a set of rules and computes their cost.
// Rules are pre-sorted by specificity: exact server+tool wins over a glob
// server with an exact tool, which wins over any server+glob tool; ties are
// broken by rule key ascending.
type Resolver struct {
	rules []Rule
}

// NewResolver builds a Resolver from rules, sorting them by specificity.
func NewResolver(rules []Rule) *Resolver {
	sorted := make([]Rule, len(rules))
	copy(sorted, rules)
	sort.SliceStable(sorted, func(i, j int) bool {
		if si, sj := ruleSpecificity(sorted[i]), ruleSpecificity(sorted[j]); si != sj {
			return si > sj
		}
		return sorted[i].Key < sorted[j].Key
	})
	return &Resolver{rules: sorted}
}

// ruleSpecificity returns a higher score for a more specific rule: an exact
// pattern beats a glob pattern beats "*". Server and tool are scored
// independently and combined so "exact server + exact tool" outranks "glob
// server + exact tool" outranks "any server + glob tool".
func ruleSpecificity(r Rule) int {
	return patternSpecificity(r.Server)*3 + patternSpecificity(r.Tool)
}

func patternSpecificity(p string) int {
	switch {
	case p == "" || p == "*":
		return 0
	case strings.ContainsAny(p, "*?["):
		return 1
	default:
		return 2
	}
}

// CostFor returns the USD cost of one tool call, or 0 when no rule matches
// (no accounting for un-priced tools). For "page"/"url" units the number of
// units is counted from the unit_source argument, capped by unit_cap.
func (r *Resolver) CostFor(serverID, toolName string, arguments json.RawMessage) float64 {
	for _, rule := range r.rules {
		if !patternMatches(rule.Server, serverID) || !patternMatches(rule.Tool, toolName) {
			continue
		}
		units := 1
		if rule.Unit == UnitPage || rule.Unit == UnitURL {
			units = countUnits(arguments, rule.UnitSource)
		}
		if rule.UnitCap > 0 && units > rule.UnitCap {
			units = rule.UnitCap
		}
		if units < 0 {
			units = 1
		}
		return rule.CostPerUnit * float64(units)
	}
	return 0
}

// patternMatches reports whether pattern matches value: "*" or empty matches
// anything, otherwise path.Match glob semantics.
func patternMatches(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, value)
	return err == nil && ok
}

// countUnits derives the number of billable units from the call arguments.
// The unit_source argument may be an array (its length), a number, or a
// comma/newline-separated string. Missing unit_source or a missing argument
// counts as 1 (the whole call).
func countUnits(arguments json.RawMessage, unitSource string) int {
	if unitSource == "" || len(arguments) == 0 {
		return 1
	}
	var args map[string]any
	if err := json.Unmarshal(arguments, &args); err != nil {
		return 1
	}
	v, ok := args[unitSource]
	if !ok {
		return 1
	}
	switch val := v.(type) {
	case []any:
		return len(val)
	case float64:
		return int(val)
	case string:
		parts := strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == '\n' })
		if len(parts) > 1 {
			return len(parts)
		}
		return 1
	default:
		return 1
	}
}
