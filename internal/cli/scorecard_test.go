package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateScorecardFlag(t *testing.T) {
	validateCmd, ok := FindCommand("validate")
	if !ok {
		t.Fatalf("validate command not registered")
	}

	// 1. Test validate --scorecard (human output)
	stdout := captureOutput(func() {
		validateCmd.Run([]string{"../../knowledge", "--scorecard"})
	})
	if !strings.Contains(stdout, "Quality Scorecard:") {
		t.Errorf("Expected stdout to contain Quality Scorecard section, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Epistemic Maintenance:") || !strings.Contains(stdout, "Human Stewardship:") {
		t.Errorf("Expected stdout to contain pillars, got:\n%s", stdout)
	}

	// 2. Test validate --scorecard --json
	stdoutJSON := captureOutput(func() {
		validateCmd.Run([]string{"../../knowledge", "--scorecard", "--json"})
	})

	var valRes struct {
		IsConformant bool `json:"is_conformant"`
		GatePassed   bool `json:"gate_passed"`
		Scorecard    *struct {
			Score int    `json:"score"`
			Grade string `json:"grade"`
		} `json:"scorecard"`
	}
	if err := json.Unmarshal([]byte(stdoutJSON), &valRes); err != nil {
		t.Fatalf("Failed to parse validate json: %v\nOutput:\n%s", err, stdoutJSON)
	}
	if valRes.Scorecard == nil {
		t.Fatalf("Expected scorecard in validate json output")
	}
	if valRes.Scorecard.Score <= 0 {
		t.Errorf("Expected positive scorecard score, got: %d", valRes.Scorecard.Score)
	}

	// 3. Test validate --min-score (passing threshold)
	stdoutMinScore := captureOutput(func() {
		validateCmd.Run([]string{"../../knowledge", "--min-score", "50", "--json"})
	})
	var minRes struct {
		GatePassed bool `json:"gate_passed"`
		Scorecard  *struct {
			Score int `json:"score"`
		} `json:"scorecard"`
	}
	if err := json.Unmarshal([]byte(stdoutMinScore), &minRes); err != nil {
		t.Fatalf("Failed to parse min-score json: %v", err)
	}
	if !minRes.GatePassed {
		t.Errorf("Expected GatePassed=true when min-score=50 and score=%d", minRes.Scorecard.Score)
	}
}
