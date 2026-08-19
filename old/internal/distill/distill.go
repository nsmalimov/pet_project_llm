// Package distill turns a session + friction report into concrete,
// CLAUDE.md-ready rules via an LLM pass.
package distill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hindsight/internal/analyze"
	"hindsight/internal/llm"
	"hindsight/internal/session"
)

type Rule struct {
	Rule     string `json:"rule"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"`
}

type Friction struct {
	Description string `json:"description"`
	Evidence    string `json:"evidence"`
}

type Result struct {
	Summary   string     `json:"summary"`
	Frictions []Friction `json:"frictions"`
	Rules     []Rule     `json:"rules"`
}

const systemPrompt = `You are "hindsight", a retrospective analyst for AI coding-agent sessions.
You receive a compacted transcript of one Claude Code session plus a deterministic friction report.

Your job: extract durable, project-specific lessons so the agent does NOT repeat the same mistakes in future sessions.

Focus on:
- moments where the USER corrected or redirected the agent (these are gold — turn them into rules);
- error loops: commands/approaches that failed repeatedly and what finally worked;
- permission denials: what the user refused to allow;
- missing project knowledge the agent had to discover the hard way (build commands, package manager, conventions, test invocation, directory layout).

Rules must be:
- imperative, one line, immediately usable in CLAUDE.md (e.g. "Use pnpm, not npm — npm install breaks the lockfile");
- project-specific and non-obvious (NO generic advice like "write tests" or "read the code first");
- justified by concrete evidence from THIS transcript.

If the session was smooth and there is nothing durable to learn, return an empty rules array — do not invent rules.

Respond with ONLY a JSON object, no markdown fences, matching:
{"summary": "2-3 sentence session summary",
 "frictions": [{"description": "...", "evidence": "quote or paraphrase from transcript"}],
 "rules": [{"rule": "one-line CLAUDE.md rule", "reason": "why", "evidence": "what in the transcript proves it"}]}
The user's language for summary/descriptions/reasons should match the language the user wrote in; rules themselves in English.`

// Distill runs the LLM pass over a session.
func Distill(ctx context.Context, s *session.Session, r *analyze.Report, model string) (*Result, error) {
	prompt := systemPrompt + "\n\n=== FRICTION REPORT ===\n" + analyze.Render(r) +
		"\n\n=== TRANSCRIPT (compacted) ===\n" + s.Compact(60000)

	raw, err := llm.Complete(ctx, model, prompt)
	if err != nil {
		return nil, err
	}
	jsonStr := extractJSON(raw)
	var res Result
	if err := json.Unmarshal([]byte(jsonStr), &res); err != nil {
		return nil, fmt.Errorf("LLM returned unparseable output: %w\n--- raw ---\n%s", err, raw)
	}
	return &res, nil
}

// extractJSON tolerates markdown fences or prose around the JSON object.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// Render pretty-prints a distill result for the terminal.
func Render(res *Result) string {
	var b strings.Builder
	b.WriteString("Summary:\n  " + wrap(res.Summary, 100, "  ") + "\n")
	if len(res.Frictions) > 0 {
		b.WriteString("\nFrictions:\n")
		for _, f := range res.Frictions {
			fmt.Fprintf(&b, "  • %s\n      evidence: %s\n", wrap(f.Description, 96, "    "), wrap(f.Evidence, 90, "      "))
		}
	}
	if len(res.Rules) == 0 {
		b.WriteString("\nNo durable rules extracted — nothing worth adding to CLAUDE.md.\n")
		return b.String()
	}
	b.WriteString("\nProposed CLAUDE.md rules:\n")
	for i, r := range res.Rules {
		fmt.Fprintf(&b, "  %d. %s\n     why: %s\n", i+1, r.Rule, wrap(r.Reason, 90, "     "))
	}
	return b.String()
}

const marker = "<!-- added by hindsight"

// WriteRules appends new rules to <projectDir>/CLAUDE.md, skipping rules the
// file already contains. Returns the number of rules written.
func WriteRules(projectDir string, sessionID string, rules []Rule) (int, string, error) {
	path := filepath.Join(projectDir, "CLAUDE.md")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, path, err
	}
	var fresh []Rule
	for _, r := range rules {
		if !strings.Contains(string(existing), r.Rule) {
			fresh = append(fresh, r)
		}
	}
	if len(fresh) == 0 {
		return 0, path, nil
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	if len(existing) == 0 {
		b.WriteString("# CLAUDE.md\n")
	}
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	fmt.Fprintf(&b, "\n%s — session %s, %s -->\n", marker, short, time.Now().Format("2006-01-02"))
	for _, r := range fresh {
		fmt.Fprintf(&b, "- %s\n", r.Rule)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return 0, path, err
	}
	return len(fresh), path, nil
}

func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	var b strings.Builder
	line := 0
	for i, w := range words {
		if line+len(w) > width && line > 0 {
			b.WriteString("\n" + indent)
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}
