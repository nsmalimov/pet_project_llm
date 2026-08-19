package session

import (
	"fmt"
	"strings"
)

// Compact renders the session as a size-bounded plain-text log suitable for
// feeding to an LLM. User messages are kept the fullest; tool results are
// trimmed the hardest (errors kept longer than successes).
func (s *Session) Compact(budget int) string {
	if budget <= 0 {
		budget = 60000
	}
	var lines []string
	for _, ev := range s.Events {
		if ev.IsMeta {
			continue
		}
		switch ev.Kind {
		case UserText:
			lines = append(lines, "[USER] "+trim(ev.Text, 2500))
		case AssistantText:
			lines = append(lines, "[ASSISTANT] "+trim(ev.Text, 1200))
		case ToolCall:
			lines = append(lines, fmt.Sprintf("[TOOL %s] %s", ev.ToolName, trim(ev.ToolInput, 400)))
		case ToolResult:
			if ev.IsError {
				lines = append(lines, "[RESULT ERROR] "+trim(ev.Text, 800))
			} else {
				lines = append(lines, "[RESULT ok] "+trim(ev.Text, 300))
			}
		}
	}
	out := strings.Join(lines, "\n")
	if len(out) <= budget {
		return out
	}
	// Keep head and tail; the middle of long sessions is usually grind.
	head := budget * 2 / 3
	tail := budget - head
	return out[:head] + "\n[... middle of session truncated ...]\n" + out[len(out)-tail:]
}

func trim(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
