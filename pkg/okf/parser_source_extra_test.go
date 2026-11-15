package okf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/okf"
)

func TestSourceUnknownKeysSurviveRoundTrip(t *testing.T) {
	raw := `---
type: Fact
sources:
  - resource: https://example.org/paper
    title: An example source
    checksum: 9f86d081884c
    accessed: 2026-01-01
---

# Fact
`
	first, err := okf.ParseConcept("test.md", raw)
	if err != nil {
		t.Fatalf("ParseConcept() failed: %v", err)
	}
	parsed, err := json.Marshal(first.Sources)
	if err != nil {
		t.Fatalf("json.Marshal() failed: %v", err)
	}
	for _, kept := range []string{`"accessed":"2026-01-01"`, `"checksum":"9f86d081884c"`} {
		if !strings.Contains(string(parsed), kept) {
			t.Fatalf("parsed sources lost %s: %s", kept, parsed)
		}
	}
	serialized := okf.SerializeConcept(first)
	for _, line := range []string{"    accessed: 2026-01-01", "    checksum: 9f86d081884c"} {
		if !strings.Contains(serialized, line) {
			t.Fatalf("serialized concept lost %q:\n%s", line, serialized)
		}
	}
	second, err := okf.ParseConcept("test.md", serialized)
	if err != nil {
		t.Fatalf("second ParseConcept() failed: %v", err)
	}
	if !reflect.DeepEqual(second.Sources, first.Sources) {
		t.Fatalf("sources drifted across a round trip: %#v != %#v", second.Sources, first.Sources)
	}
}

func TestSourceUnknownKeysSurviveUpdateConcept(t *testing.T) {
	tmpDir := t.TempDir()
	bundleDir := filepath.Join(tmpDir, "knowledge")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(bundleDir, "index.md"), []byte("---\nokf_version: \"0.2\"\n---\n# Root\n"), 0o644)
	_ = os.WriteFile(filepath.Join(bundleDir, "log.md"), []byte("# Log\n"), 0o644)

	raw := `---
type: Note
title: Note with custom source keys
sources:
  - resource: https://example.org/paper
    title: Source Paper
    checksum: abc123def
    accessed: 2026-02-02
---

# Note Body
`
	if err := os.WriteFile(filepath.Join(bundleDir, "note.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := okf.LoadBundle(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	c := b.Concepts["note"]
	c.Description = "Updated description"
	if err := okf.SaveConcept(bundleDir, c, okf.SaveOptions{IsNew: false, Actor: "human:tester"}); err != nil {
		t.Fatalf("SaveConcept() failed: %v", err)
	}

	updatedData, err := os.ReadFile(filepath.Join(bundleDir, "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(updatedData)
	if !strings.Contains(content, "checksum: abc123def") || !strings.Contains(content, "accessed: 2026-02-02") {
		t.Fatalf("UpdateConcept lost unknown keys in sources:\n%s", content)
	}
}

func TestStaleAfterNullPreservedForValidation(t *testing.T) {
	raw := `---
type: Fact
stale_after: null
---

# Fact
`
	c, err := okf.ParseConcept("test.md", raw)
	if err != nil {
		t.Fatalf("ParseConcept() failed: %v", err)
	}
	if c.StaleAfter != "null" {
		t.Fatalf("stale_after: expected literal %q to trigger validation warning, got %q", "null", c.StaleAfter)
	}
}
