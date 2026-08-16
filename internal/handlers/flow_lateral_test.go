package handlers

import (
	"strings"
	"testing"
)

// The canary is what cmd/sqlcheck actually PREPAREs (the spliced queries are
// concat-skipped), so it must contain the shipped fragments byte-for-byte —
// otherwise sqlcheck would be validating text we don't run.
func TestFlowBandCanaryContainsFragments(t *testing.T) {
	if !strings.Contains(flowBandLateralCanary, flowBandLateralSQL) {
		t.Fatal("flowBandLateralCanary does not contain flowBandLateralSQL verbatim — keep them byte-identical")
	}
	if !strings.Contains(flowBandLateralCanary, strings.TrimSpace(inBandPredicateSQL)) {
		t.Fatal("flowBandLateralCanary does not contain inBandPredicateSQL — keep them in sync")
	}
}
