package handlers

import (
	"regexp"
	"strings"
)

// FlowBandThreshold is a single CFS threshold in the flexible band model.
// Band applies when cfs >= Value. See V7, V9, V11.
type FlowBandThreshold struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
	Color string  `json:"color"`
}

// FlowBands is the full band config for a reach: base (no threshold) + 0..8 thresholds.
// GET and PUT /flow-ranges both use this shape (V15).
type FlowBands struct {
	BaseLabel  string              `json:"base_label"`
	BaseColor  string              `json:"base_color"`
	Thresholds []FlowBandThreshold `json:"thresholds"`
}

// colorKeyRe accepts both color formats a flow-band color may carry:
//   - indexed palette: "p<n>" where n is 0..39 (8 families × 5 levels, web#252)
//   - legacy family-level keys: "green-3" etc. (still present on older rows)
// The indexed form is now the default the web client sends; the migration in
// web#252 moved stored keys to "p<n>", so the validator must accept it or every
// edit-save 400s (issue #307). Legacy list includes pink (previously omitted).
var colorKeyRe = regexp.MustCompile(`^(p([0-9]|[12][0-9]|3[0-9])|(red|orange|yellow|green|blue|purple|pink|neutral)-[1-5])$`)

// flowStatusFromColor maps a color key to the legacy flow_status enum (V9).
// red-* → caution, blue-* → flood, other color → runnable, empty → unknown.
func flowStatusFromColor(color string) string {
	switch {
	case strings.HasPrefix(color, "red"):
		return "caution"
	case strings.HasPrefix(color, "blue"):
		return "flood"
	case color != "":
		return "runnable"
	default:
		return "unknown"
	}
}

// validateFlowBands enforces V16 constraints on a PUT body.
func validateFlowBands(fb FlowBands) string {
	if !colorKeyRe.MatchString(fb.BaseColor) {
		return "base_color must match color key format (e.g. red-3)"
	}
	if strings.TrimSpace(fb.BaseLabel) == "" {
		return "base_label is required"
	}
	if len(fb.Thresholds) > 8 {
		return "maximum 8 thresholds allowed"
	}
	seen := make(map[float64]bool)
	for _, t := range fb.Thresholds {
		if t.Value < 0 {
			return "threshold values must be >= 0"
		}
		if seen[t.Value] {
			return "duplicate threshold values are not allowed"
		}
		seen[t.Value] = true
		if l := strings.TrimSpace(t.Label); l == "" || len(l) > 32 {
			return "threshold labels must be 1..32 characters"
		}
		if !colorKeyRe.MatchString(t.Color) {
			return "threshold colors must match color key format (e.g. green-3)"
		}
	}
	return ""
}
