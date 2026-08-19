// Package roles defines the agent roles. A role is not a persona: each one
// has its own goal, its own slice of context, its own instructions, its own
// structured output contract and its own permissions.
//
// Notably the Reviewer receives the requirements and the diff — never the
// developer's reasoning — so its review is genuinely independent.
package roles

import (
	"encoding/json"
	"fmt"
	"strings"

	"orchestrator/internal/domain"
)

const (
	Researcher = "researcher"
	Developer  = "developer"
	Reviewer   = "reviewer"
	Tester     = "tester"
)

// ReadOnly reports whether a role is allowed to modify files.
func ReadOnly(role string) bool { return role == Researcher || role == Reviewer }

// Input carries everything a prompt builder may need. Builders pick only what
// their role is entitled to see.
type Input struct {
	Task  *domain.Task
	Rules []string // persistent memory: project rules & user preferences

	// Researcher (investigation mode)
	InvestigationQuestion string

	// Developer
	ResearchSummary string
	KeyFiles        []string
	TestFailures    []domain.TestResult
	ReviewFindings  []domain.Finding

	// Reviewer
	Diff         string
	ChangedFiles []string
}

func repoList(t *domain.Task) string {
	var sb strings.Builder
	for _, r := range t.Repos {
		fmt.Fprintf(&sb, "  - %s/ (worktree of %s)\n", r.Name, r.Path)
	}
	return sb.String()
}

func commonHeader(t *domain.Task, rules []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task: %s\n\n", t.Goal)
	if len(t.Context) > 0 {
		sb.WriteString("Additional context:\n")
		for _, c := range t.Context {
			fmt.Fprintf(&sb, "  - %s\n", c)
		}
		sb.WriteString("\n")
	}
	if len(t.State.Notes) > 0 {
		sb.WriteString("Human guidance / resolved decisions (authoritative):\n")
		for _, n := range t.State.Notes {
			fmt.Fprintf(&sb, "  - %s\n", n)
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "Repositories in the current working directory:\n%s\n", repoList(t))
	if len(rules) > 0 {
		sb.WriteString("Project rules and user preferences (follow them):\n")
		for _, r := range rules {
			fmt.Fprintf(&sb, "  - %s\n", r)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

const decisionRequestSchema = `"decision_request": null OR {"importance":"low|medium|high","question":"...","recommendation":"...","reason":"...","options":[{"id":"a","label":"...","detail":"..."}]}`

// ResearcherPrompt: understand the codebase(s) well enough for a developer to
// implement the task. In investigation mode, answer a specific question.
func ResearcherPrompt(in Input) string {
	var sb strings.Builder
	sb.WriteString("You are the Researcher agent of an automated engineering orchestrator.\n")
	if in.InvestigationQuestion != "" {
		sb.WriteString("Mode: DEEP INVESTIGATION. A previous step got blocked; your job is to resolve the specific question below with concrete findings from the code.\n\n")
	} else {
		sb.WriteString("Mode: initial understanding. Your job is to understand the code well enough that a developer can implement the task safely.\n\n")
	}
	sb.WriteString(commonHeader(in.Task, in.Rules))
	if in.InvestigationQuestion != "" {
		fmt.Fprintf(&sb, "Question to investigate:\n%s\n\n", in.InvestigationQuestion)
	}
	sb.WriteString(`Investigate by reading and searching the code. Do NOT modify any files.

When done, end your reply with exactly one JSON object in a ` + "```json" + ` fence:
{
  "summary": "what the task requires, how the relevant code works, where changes should go",
  "key_files": ["repoName/path/to/file", ...],
  "uncertainty": "low|medium|high",
  "risks": ["..."],
  "open_questions": ["..."],
  ` + decisionRequestSchema + `
}

uncertainty=high means: implementing now without more information would likely produce a wrong result.
Use decision_request ONLY for questions a human must answer (ambiguous product intent, destructive/irreversible choices) — not for things you can find in the code.
`)
	return sb.String()
}

// DeveloperPrompt: make the change. Receives research findings and, on
// retries, test failures or review findings.
func DeveloperPrompt(in Input) string {
	var sb strings.Builder
	sb.WriteString("You are the Developer agent of an automated engineering orchestrator.\n")
	sb.WriteString("Your job: implement the task in the isolated git worktrees in the current directory. Edit freely; do not push, do not switch branches.\n\n")
	sb.WriteString(commonHeader(in.Task, in.Rules))
	if in.ResearchSummary != "" {
		fmt.Fprintf(&sb, "Findings from the Researcher:\n%s\n", in.ResearchSummary)
		if len(in.KeyFiles) > 0 {
			fmt.Fprintf(&sb, "Key files: %s\n", strings.Join(in.KeyFiles, ", "))
		}
		sb.WriteString("\n")
	}
	if len(in.TestFailures) > 0 {
		sb.WriteString("Your previous change FAILED verification. Fix the following:\n")
		for _, f := range in.TestFailures {
			fmt.Fprintf(&sb, "--- repo %s, command `%s` (exit %d):\n%s\n", f.Repo, f.Command, f.ExitCode, f.OutputTail)
		}
		sb.WriteString("\n")
	}
	if len(in.ReviewFindings) > 0 {
		sb.WriteString("An independent reviewer requested changes. Address every finding:\n")
		for _, f := range in.ReviewFindings {
			fmt.Fprintf(&sb, "  - [%s] %s: %s\n", f.Severity, f.File, f.Issue)
		}
		sb.WriteString("\n")
	}
	sb.WriteString(`Keep the change minimal and focused on the task. Add or update tests when the task is a behaviour change or bug fix. Run the relevant build/tests if they are quick.

When done, end your reply with exactly one JSON object in a ` + "```json" + ` fence:
{
  "status": "completed|blocked|uncertain",
  "summary": "what you changed and why",
  "files_changed": ["repoName/path", ...],
  "notes": "anything the orchestrator should know (e.g. what blocks you)",
  ` + decisionRequestSchema + `
}

status=blocked: you cannot proceed without information you could not find — state precisely what is missing in notes (it becomes an investigation task) or raise a decision_request if only a human can answer.
status=uncertain: you made a change but are not confident it is correct.
`)
	return sb.String()
}

// ReviewerPrompt: independent verification. Gets requirements + diff + read
// access to the worktree. Deliberately does NOT include the developer's
// summary, notes or reasoning.
func ReviewerPrompt(in Input) string {
	var sb strings.Builder
	sb.WriteString("You are an independent code Reviewer. You did NOT write this change and you have no access to the author's reasoning — verify everything yourself against the requirements.\n\n")
	sb.WriteString(commonHeader(in.Task, in.Rules))
	sb.WriteString("The change under review (diff against the base revision):\n")
	sb.WriteString("```diff\n" + in.Diff + "\n```\n\n")
	sb.WriteString(`The full working tree is available read-only in the current directory — read any file you need for context.

Check:
  1. Does the diff correctly and completely satisfy the task requirements?
  2. Correctness bugs, edge cases, broken behaviour.
  3. Unintended or unrelated changes.
  4. Missing or inadequate tests for the changed behaviour.

Request changes only for real problems; do not block on style preferences.

End your reply with exactly one JSON object in a ` + "```json" + ` fence:
{
  "verdict": "approve|changes_requested",
  "summary": "your independent assessment",
  "findings": [{"severity":"high|medium|low","file":"repoName/path","issue":"..."}]
}
`)
	return sb.String()
}

// ---------- output parsing ----------

// ExtractJSON finds the last JSON object in an agent's reply. It prefers
// fenced ```json blocks and falls back to the last brace-balanced object.
func ExtractJSON(s string) (string, error) {
	// Preferred: fenced blocks, last one first.
	parts := strings.Split(s, "```")
	for i := len(parts) - 1; i >= 0; i-- {
		block := strings.TrimSpace(parts[i])
		block = strings.TrimPrefix(block, "json")
		block = strings.TrimSpace(block)
		if strings.HasPrefix(block, "{") && json.Valid([]byte(block)) {
			return block, nil
		}
	}
	// Fallback: last '{' that starts a valid JSON value.
	for i := strings.LastIndex(s, "{"); i >= 0; i = strings.LastIndex(s[:i], "{") {
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil && len(raw) > 0 && raw[0] == '{' {
			return string(raw), nil
		}
	}
	return "", fmt.Errorf("no JSON object found in agent output")
}

func parseInto[T any](output string) (*T, error) {
	js, err := ExtractJSON(output)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		return nil, fmt.Errorf("agent output JSON does not match schema: %w", err)
	}
	return &v, nil
}

func ParseResearch(output string) (*domain.ResearchOutput, error) {
	out, err := parseInto[domain.ResearchOutput](output)
	if err != nil {
		return nil, err
	}
	switch out.Uncertainty {
	case "low", "medium", "high":
	case "":
		out.Uncertainty = "medium"
	default:
		out.Uncertainty = "high" // unknown value → be conservative
	}
	return out, nil
}

func ParseDevelop(output string) (*domain.DevelopOutput, error) {
	out, err := parseInto[domain.DevelopOutput](output)
	if err != nil {
		return nil, err
	}
	switch out.Status {
	case "completed", "blocked", "uncertain":
	default:
		return nil, fmt.Errorf("developer output has invalid status %q", out.Status)
	}
	return out, nil
}

func ParseReview(output string) (*domain.ReviewOutput, error) {
	out, err := parseInto[domain.ReviewOutput](output)
	if err != nil {
		return nil, err
	}
	switch out.Verdict {
	case "approve", "changes_requested":
	default:
		return nil, fmt.Errorf("reviewer output has invalid verdict %q", out.Verdict)
	}
	return out, nil
}
