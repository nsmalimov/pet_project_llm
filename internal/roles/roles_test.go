package roles

import (
	"strings"
	"testing"

	"orchestrator/internal/domain"
)

func TestExtractJSONFenced(t *testing.T) {
	out := "I looked around.\n```json\n{\"summary\":\"ok\",\"uncertainty\":\"low\"}\n```\n"
	js, err := ExtractJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js, `"summary"`) {
		t.Fatalf("unexpected: %s", js)
	}
}

func TestExtractJSONBare(t *testing.T) {
	out := `Some prose. {"verdict":"approve","summary":"fine","findings":[]} trailing`
	js, err := ExtractJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseReview(js)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "approve" {
		t.Fatalf("verdict=%s", r.Verdict)
	}
}

func TestExtractJSONPrefersLast(t *testing.T) {
	out := "```json\n{\"a\":1}\n```\ntext\n```json\n{\"verdict\":\"approve\",\"summary\":\"x\"}\n```"
	js, err := ExtractJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js, "verdict") {
		t.Fatalf("expected last block, got %s", js)
	}
}

func TestExtractJSONNone(t *testing.T) {
	if _, err := ExtractJSON("no json here"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseDevelopValidation(t *testing.T) {
	if _, err := ParseDevelop(`{"status":"weird","summary":"x"}`); err == nil {
		t.Fatal("expected invalid status error")
	}
	d, err := ParseDevelop(`{"status":"blocked","summary":"x","notes":"missing config"}`)
	if err != nil {
		t.Fatal(err)
	}
	if d.Notes != "missing config" {
		t.Fatal("notes lost")
	}
}

func TestParseResearchUnknownUncertaintyIsConservative(t *testing.T) {
	r, err := ParseResearch(`{"summary":"x","uncertainty":"banana"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Uncertainty != "high" {
		t.Fatalf("uncertainty=%s, want high", r.Uncertainty)
	}
}

func TestReviewerPromptExcludesDeveloperReasoning(t *testing.T) {
	task := &domain.Task{
		Goal:  "fix retry",
		Repos: []domain.RepoRef{{Name: "a", Path: "/x/a"}},
		State: domain.TaskState{DeveloperSummary: "SECRET-DEV-REASONING"},
	}
	p := ReviewerPrompt(Input{Task: task, Diff: "diff --git ..."})
	if strings.Contains(p, "SECRET-DEV-REASONING") {
		t.Fatal("reviewer prompt leaked developer reasoning")
	}
	if !strings.Contains(p, "diff --git") {
		t.Fatal("reviewer prompt missing diff")
	}
}
