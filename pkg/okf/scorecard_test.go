package okf

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScorecardPerfectBundle(t *testing.T) {
	dir := t.TempDir()
	if err := InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}

	c1 := &Concept{
		ID:          "decisions/caching",
		Path:        "decisions/caching.md",
		Type:        "Decision",
		Title:       "Caching Invariants",
		Description: "Durable caching invariant without stale_after.",
		Status:      "stable",
		Generated:   &Generated{By: "human/alice", At: time.Now().UTC().Format(time.RFC3339)},
		Verified:    []Verified{{By: "human/bob", At: time.Now().UTC().Add(time.Minute).Format(time.RFC3339)}},
		Body:        "# Caching\n\nBody content.",
	}
	if err := SaveConcept(dir, c1, SaveOptions{IsNew: true, AutoIndex: true, Actor: "human/alice"}); err != nil {
		t.Fatalf("SaveConcept c1: %v", err)
	}

	c2 := &Concept{
		ID:          "decisions/routing",
		Path:        "decisions/routing.md",
		Type:        "Decision",
		Title:       "Routing Rules",
		Description: "Routing invariant linked to caching.",
		Status:      "stable",
		Generated:   &Generated{By: "human/bob", At: time.Now().UTC().Format(time.RFC3339)},
		Verified:    []Verified{{By: "human/charlie", At: time.Now().UTC().Add(time.Minute).Format(time.RFC3339)}},
		Body:        "# Routing\n\nSee [Caching](caching.md).",
	}
	if err := SaveConcept(dir, c2, SaveOptions{IsNew: true, AutoIndex: true, Actor: "human/bob"}); err != nil {
		t.Fatalf("SaveConcept c2: %v", err)
	}

	// Link back to avoid orphan
	if err := RelateConcepts(dir, "decisions/caching", "decisions/routing", "governs routing", "human/alice"); err != nil {
		t.Fatalf("RelateConcepts: %v", err)
	}

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	sc := ComputeScorecard(b)
	if sc.Grade != "A" {
		t.Errorf("Expected Grade A for perfect bundle, got %s (Score: %d)", sc.Grade, sc.Score)
	}
	if sc.Maintenance != 100 {
		t.Errorf("Expected Maintenance 100, got %d", sc.Maintenance)
	}
	if sc.Stewardship != 100 {
		t.Errorf("Expected Stewardship 100, got %d", sc.Stewardship)
	}
	if sc.Integrity != 100 {
		t.Errorf("Expected Integrity 100, got %d", sc.Integrity)
	}
	if sc.Score != 100 {
		t.Errorf("Expected overall Score 100, got %d", sc.Score)
	}
}

func TestScorecardLapsedReviewPenalty(t *testing.T) {
	dir := t.TempDir()
	if err := InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}

	// Expired stale_after date
	c1 := &Concept{
		ID:          "runbooks/deploy",
		Path:        "runbooks/deploy.md",
		Type:        "Runbook",
		Title:       "Deployment",
		Description: "Expired deployment steps.",
		Status:      "stable",
		StaleAfter:  "2020-01-01", // Long expired
		Generated:   &Generated{By: "human/alice", At: "2019-01-01T00:00:00Z"},
		Verified:    []Verified{{By: "human/alice", At: "2019-01-02T00:00:00Z"}},
		Body:        "# Deploy\n\nContent.",
	}
	if err := SaveConcept(dir, c1, SaveOptions{IsNew: true, AutoIndex: true, Actor: "human/alice"}); err != nil {
		t.Fatalf("SaveConcept c1: %v", err)
	}

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	sc := ComputeScorecard(b)
	if sc.Maintenance != 0 {
		t.Errorf("Expected Maintenance 0 for expired concept, got %d", sc.Maintenance)
	}
	if sc.Score >= 90 {
		t.Errorf("Expected Score to drop due to lapsed review, got %d", sc.Score)
	}
}

func TestScorecardSyntheticAgentDump(t *testing.T) {
	dir := t.TempDir()
	if err := InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}

	c1 := &Concept{
		ID:          "facts/db",
		Path:        "facts/db.md",
		Type:        "Fact",
		Title:       "Database Fact",
		Description: "Generated entirely by an AI agent without human review.",
		Status:      "stable",
		Generated:   &Generated{By: "agent/synthetic-bot", At: "2026-01-01T00:00:00Z"},
		Body:        "# DB\n\nRaw bot content.",
	}
	if err := SaveConcept(dir, c1, SaveOptions{IsNew: true, AutoIndex: true, Actor: "agent/synthetic-bot"}); err != nil {
		t.Fatalf("SaveConcept c1: %v", err)
	}

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	sc := ComputeScorecard(b)
	if sc.Stewardship != 20 {
		t.Errorf("Expected Stewardship 20 for unreviewed synthetic agent dump, got %d", sc.Stewardship)
	}
	if sc.Score >= 80 {
		t.Errorf("Expected Score below 80 for unreviewed bot dump, got %d", sc.Score)
	}
}

func TestScorecardRootDeprecatedZeroesMaintenance(t *testing.T) {
	dir := t.TempDir()
	if err := InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}

	// Write root index with status: deprecated
	idxContent := `---
okf_version: "0.2"
status: deprecated
---

# Deprecated Bundle
`
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(idxContent), 0o644); err != nil {
		t.Fatalf("WriteFile index.md: %v", err)
	}

	c1 := &Concept{
		ID:          "facts/old",
		Path:        "facts/old.md",
		Type:        "Fact",
		Title:       "Old Fact",
		Description: "Old fact in deprecated bundle.",
		Status:      "stable",
		Generated:   &Generated{By: "human/alice", At: "2026-01-01T00:00:00Z"},
		Body:        "# Old\n\nContent.",
	}
	if err := SaveConcept(dir, c1, SaveOptions{IsNew: true, AutoIndex: true, Actor: "human/alice"}); err != nil {
		t.Fatalf("SaveConcept c1: %v", err)
	}

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	sc := ComputeScorecard(b)
	if sc.Maintenance != 0 {
		t.Errorf("Expected Maintenance 0 for deprecated bundle, got %d", sc.Maintenance)
	}
}

func TestScorecardEmptyBundle(t *testing.T) {
	b := &Bundle{
		Concepts: make(map[string]*Concept),
	}
	sc := ComputeScorecard(b)
	if sc.Score != 0 || sc.Grade != "F" {
		t.Errorf("Expected Score 0 and Grade F for empty bundle, got Score: %d, Grade: %s", sc.Score, sc.Grade)
	}
}

func TestValidateWithScorecardOption(t *testing.T) {
	dir := t.TempDir()
	if err := InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}
	c1 := &Concept{
		ID:          "facts/alpha",
		Path:        "facts/alpha.md",
		Type:        "Fact",
		Title:       "Alpha",
		Description: "Alpha fact.",
		Status:      "stable",
		Generated:   &Generated{By: "human/alice", At: time.Now().UTC().Format(time.RFC3339)},
		Body:        "# Alpha\n\nContent.",
	}
	if err := SaveConcept(dir, c1, SaveOptions{IsNew: true, AutoIndex: true, Actor: "human/alice"}); err != nil {
		t.Fatalf("SaveConcept c1: %v", err)
	}

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	res := Validate(b, ValidateOptions{Scorecard: true})
	if res.Scorecard == nil {
		t.Fatalf("Expected res.Scorecard to be populated when Scorecard=true")
	}
	if res.Scorecard.Score == 0 {
		t.Errorf("Expected non-zero scorecard score")
	}
}

func TestScorecardRejectsNonHumanVerificationStewardship(t *testing.T) {
	dir := t.TempDir()
	if err := InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}

	// Concept with agent generation and non-human verification
	c1 := &Concept{
		ID:          "facts/peer-checked",
		Path:        "facts/peer-checked.md",
		Type:        "Fact",
		Title:       "Peer Checked Fact",
		Description: "Concept generated by an agent and verified by another agent/bot.",
		Status:      "stable",
		Generated:   &Generated{By: "agent/author-bot", At: "2026-01-01T00:00:00Z"},
		Verified:    []Verified{{By: "agent/reviewer-bot", At: "2026-01-02T00:00:00Z"}},
		Body:        "# Fact\n\nPeer bot content.",
	}
	if err := SaveConcept(dir, c1, SaveOptions{IsNew: true, AutoIndex: true, Actor: "agent/author-bot"}); err != nil {
		t.Fatalf("SaveConcept c1: %v", err)
	}

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	sc := ComputeScorecard(b)
	// Non-human verification must NOT earn stewardship points; remains at baseline synthetic agent dump (20)
	if sc.Stewardship != 20 {
		t.Errorf("Expected Stewardship 20 for non-human verification, got %d", sc.Stewardship)
	}
}

func TestValidateMinScoreGate(t *testing.T) {
	dir := t.TempDir()
	if err := InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}
	c1 := &Concept{
		ID:          "facts/alpha",
		Path:        "facts/alpha.md",
		Type:        "Fact",
		Title:       "Alpha",
		Description: "Alpha fact.",
		Status:      "stable",
		Generated:   &Generated{By: "agent/bot", At: time.Now().UTC().Format(time.RFC3339)},
		Body:        "# Alpha\n\nContent.",
	}
	if err := SaveConcept(dir, c1, SaveOptions{IsNew: true, AutoIndex: true, Actor: "agent/bot"}); err != nil {
		t.Fatalf("SaveConcept c1: %v", err)
	}

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	// 1. MinScore 50: Should pass
	resPass := Validate(b, ValidateOptions{MinScore: 50})
	if !resPass.GatePassed {
		t.Errorf("Expected GatePassed=true for MinScore=50, got false")
	}

	// 2. MinScore 95: Should fail (since synthetic dump scores ~62)
	resFail := Validate(b, ValidateOptions{MinScore: 95})
	if resFail.GatePassed {
		t.Errorf("Expected GatePassed=false for MinScore=95, got true")
	}
	if len(resFail.GateFindings) == 0 {
		t.Errorf("Expected gate findings for failing min-score")
	}
}
