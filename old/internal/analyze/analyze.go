// Package analyze computes a deterministic "friction report" for a session:
// error loops, interruptions, permission denials, user steering messages.
package analyze

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"hindsight/internal/session"
)

type ToolStat struct {
	Name   string
	Calls  int
	Errors int
}

// ErrorLoop is the same (tool, similar input) failing repeatedly —
// the classic "agent bangs its head against the wall" signal.
type ErrorLoop struct {
	Tool     string
	Key      string // representative input (e.g. the bash command)
	Failures int
	LastErr  string
}

type Report struct {
	Session       *session.Session
	UserTurns     int
	AssistantMsgs int
	ToolStats     []ToolStat
	ErrorLoops    []ErrorLoop
	Interruptions int
	Denials       []string // tool inputs the user refused to allow
	Steering      []string // substantive user messages after the first (corrections, redirects)
	FirstRequest  string
}

func Analyze(s *session.Session) *Report {
	r := &Report{Session: s}
	tools := map[string]*ToolStat{}
	loops := map[string]*ErrorLoop{}

	for _, ev := range s.Events {
		switch ev.Kind {
		case session.UserText:
			if ev.IsMeta {
				continue
			}
			text := strings.TrimSpace(ev.Text)
			if text == "" {
				continue
			}
			if strings.Contains(text, "[Request interrupted by user") {
				r.Interruptions++
				if rest := interruptionTail(text); rest != "" {
					r.Steering = append(r.Steering, rest)
				}
				continue
			}
			r.UserTurns++
			if r.FirstRequest == "" {
				r.FirstRequest = text
			} else {
				r.Steering = append(r.Steering, text)
			}
		case session.AssistantText:
			r.AssistantMsgs++
		case session.ToolCall:
			st := tools[ev.ToolName]
			if st == nil {
				st = &ToolStat{Name: ev.ToolName}
				tools[ev.ToolName] = st
			}
			st.Calls++
		case session.ToolResult:
			if !ev.IsError {
				continue
			}
			if st := tools[ev.ToolName]; st != nil {
				st.Errors++
			}
			if isDenial(ev.Text) {
				r.Denials = append(r.Denials, denialLabel(s, ev))
				continue
			}
			key := loopKey(s, ev)
			l := loops[key]
			if l == nil {
				l = &ErrorLoop{Tool: ev.ToolName, Key: key}
				loops[key] = l
			}
			l.Failures++
			l.LastErr = firstLine(ev.Text, 200)
		}
	}

	for _, st := range tools {
		r.ToolStats = append(r.ToolStats, *st)
	}
	sort.Slice(r.ToolStats, func(i, j int) bool { return r.ToolStats[i].Calls > r.ToolStats[j].Calls })
	for _, l := range loops {
		if l.Failures >= 2 {
			r.ErrorLoops = append(r.ErrorLoops, *l)
		}
	}
	sort.Slice(r.ErrorLoops, func(i, j int) bool { return r.ErrorLoops[i].Failures > r.ErrorLoops[j].Failures })
	return r
}

// loopKey groups similar failing calls: for Bash the first token run of the
// command, otherwise tool name + truncated input.
func loopKey(s *session.Session, res session.Event) string {
	input := inputFor(s, res.ToolID)
	if res.ToolName == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(input), &in)
		if in.Command != "" {
			return "Bash: " + firstLine(in.Command, 80)
		}
	}
	return res.ToolName + ": " + firstLine(input, 80)
}

func inputFor(s *session.Session, toolID string) string {
	for _, ev := range s.Events {
		if ev.Kind == session.ToolCall && ev.ToolID == toolID {
			return ev.ToolInput
		}
	}
	return ""
}

func isDenial(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "doesn't want to proceed") ||
		strings.Contains(t, "user rejected") ||
		strings.Contains(t, "permission to use") && strings.Contains(t, "denied")
}

func denialLabel(s *session.Session, res session.Event) string {
	return res.ToolName + " " + firstLine(inputFor(s, res.ToolID), 120)
}

func interruptionTail(text string) string {
	// "[Request interrupted by user for tool use]actual user message"
	if i := strings.LastIndex(text, "]"); i >= 0 && i+1 < len(text) {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// Render prints the report as human-readable text.
func Render(r *Report) string {
	var b strings.Builder
	s := r.Session
	fmt.Fprintf(&b, "Session %s\n", s.ID)
	fmt.Fprintf(&b, "  project: %s (branch %s)\n", s.CWD, s.GitBranch)
	if !s.Start.IsZero() {
		fmt.Fprintf(&b, "  time:    %s, duration %s\n", s.Start.Local().Format("2006-01-02 15:04"), s.Duration().Round(1e9))
	}
	if len(s.Models) > 0 {
		fmt.Fprintf(&b, "  models:  %s\n", strings.Join(s.Models, ", "))
	}
	fmt.Fprintf(&b, "  turns:   %d user / %d assistant, tokens in/out: %d/%d (+%d cache-read)\n",
		r.UserTurns, r.AssistantMsgs, s.Usage.InputTokens, s.Usage.OutputTokens, s.Usage.CacheReadTokens)
	if r.FirstRequest != "" {
		fmt.Fprintf(&b, "  task:    %s\n", firstLine(r.FirstRequest, 160))
	}

	if len(r.ToolStats) > 0 {
		b.WriteString("\nTool usage:\n")
		for _, t := range r.ToolStats {
			errs := ""
			if t.Errors > 0 {
				errs = fmt.Sprintf("  (%d errors)", t.Errors)
			}
			fmt.Fprintf(&b, "  %-22s %4d%s\n", t.Name, t.Calls, errs)
		}
	}

	friction := false
	if len(r.ErrorLoops) > 0 {
		friction = true
		b.WriteString("\nError loops (same call failing repeatedly):\n")
		for _, l := range r.ErrorLoops {
			fmt.Fprintf(&b, "  %dx  %s\n      last error: %s\n", l.Failures, l.Key, l.LastErr)
		}
	}
	if r.Interruptions > 0 {
		friction = true
		fmt.Fprintf(&b, "\nUser interrupted the agent %d time(s).\n", r.Interruptions)
	}
	if len(r.Denials) > 0 {
		friction = true
		b.WriteString("\nPermission denials:\n")
		for _, d := range r.Denials {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
	}
	if len(r.Steering) > 0 {
		b.WriteString("\nUser steering / corrections during the session:\n")
		for _, s := range r.Steering {
			fmt.Fprintf(&b, "  - %s\n", firstLine(s, 160))
		}
	}
	if !friction && len(r.Steering) == 0 {
		b.WriteString("\nNo friction detected — smooth session.\n")
	}
	return b.String()
}
