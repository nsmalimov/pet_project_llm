// Package session parses Claude Code transcript files (~/.claude/projects/<proj>/<id>.jsonl)
// into an ordered event stream that the rest of hindsight works with.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// rawEntry mirrors one JSONL line of a transcript. Only fields we care about.
type rawEntry struct {
	Type        string      `json:"type"`
	Subtype     string      `json:"subtype"`
	IsMeta      bool        `json:"isMeta"`
	IsSidechain bool        `json:"isSidechain"`
	Timestamp   string      `json:"timestamp"`
	CWD         string      `json:"cwd"`
	GitBranch   string      `json:"gitBranch"`
	SessionID   string      `json:"sessionId"`
	Message     *rawMessage `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // string or []block
	Usage   *Usage          `json:"usage"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"` // string or []{type:text,text}
}

type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

// EventKind enumerates the flattened event types.
type EventKind string

const (
	UserText      EventKind = "user"
	AssistantText EventKind = "assistant"
	ToolCall      EventKind = "tool_call"
	ToolResult    EventKind = "tool_result"
)

type Event struct {
	Kind      EventKind
	Time      time.Time
	Text      string // user/assistant text, or stringified tool result
	ToolName  string // for ToolCall and (resolved) ToolResult
	ToolInput string // compact JSON of tool input
	ToolID    string
	IsError   bool // tool_result error flag
	IsMeta    bool
}

type Session struct {
	ID        string
	Path      string
	CWD       string
	GitBranch string
	Models    []string
	Events    []Event
	Usage     Usage
	Start     time.Time
	End       time.Time
}

func (s *Session) Duration() time.Duration {
	if s.Start.IsZero() || s.End.IsZero() {
		return 0
	}
	return s.End.Sub(s.Start)
}

var localCmdRe = regexp.MustCompile(`(?s)<command-name>|<local-command-stdout>|<local-command-caveat>`)

// Load parses a transcript file.
func Load(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{Path: path}
	models := map[string]bool{}
	toolNames := map[string]string{} // tool_use_id -> name

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e rawEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // tolerate unknown/broken lines
		}
		if e.SessionID != "" && s.ID == "" {
			s.ID = e.SessionID
		}
		if e.CWD != "" {
			s.CWD = e.CWD
		}
		if e.GitBranch != "" && s.GitBranch == "" {
			s.GitBranch = e.GitBranch
		}
		if e.IsSidechain || e.Message == nil || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, e.Timestamp)
		if !ts.IsZero() {
			if s.Start.IsZero() || ts.Before(s.Start) {
				s.Start = ts
			}
			if ts.After(s.End) {
				s.End = ts
			}
		}
		if e.Message.Model != "" {
			models[e.Message.Model] = true
		}
		if e.Message.Usage != nil {
			s.Usage.InputTokens += e.Message.Usage.InputTokens
			s.Usage.OutputTokens += e.Message.Usage.OutputTokens
			s.Usage.CacheReadTokens += e.Message.Usage.CacheReadTokens
			s.Usage.CacheCreationTokens += e.Message.Usage.CacheCreationTokens
		}

		// content is either a plain string or a list of blocks
		var asString string
		if err := json.Unmarshal(e.Message.Content, &asString); err == nil {
			if e.Type == "user" && asString != "" && !localCmdRe.MatchString(asString) {
				s.Events = append(s.Events, Event{Kind: UserText, Time: ts, Text: asString, IsMeta: e.IsMeta})
			}
			continue
		}
		var blocks []block
		if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				kind := AssistantText
				if e.Type == "user" {
					kind = UserText
					if localCmdRe.MatchString(b.Text) {
						continue
					}
				}
				if strings.TrimSpace(b.Text) != "" {
					s.Events = append(s.Events, Event{Kind: kind, Time: ts, Text: b.Text, IsMeta: e.IsMeta})
				}
			case "tool_use":
				input, _ := json.Marshal(b.Input)
				toolNames[b.ID] = b.Name
				s.Events = append(s.Events, Event{Kind: ToolCall, Time: ts, ToolName: b.Name, ToolInput: string(input), ToolID: b.ID})
			case "tool_result":
				s.Events = append(s.Events, Event{
					Kind: ToolResult, Time: ts, ToolID: b.ToolUseID,
					ToolName: toolNames[b.ToolUseID],
					IsError:  b.IsError, Text: resultText(b.Content),
				})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	for m := range models {
		s.Models = append(s.Models, m)
	}
	sort.Strings(s.Models)
	if s.ID == "" {
		s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return s, nil
}

func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// ProjectsDir returns the Claude Code transcripts root.
func ProjectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// ProjectDirFor maps a working directory to its transcript folder name,
// e.g. /Users/x/my_app -> -Users-x-my-app.
func ProjectDirFor(cwd string) string {
	return nonAlnum.ReplaceAllString(cwd, "-")
}

// SessionFile describes a discovered transcript on disk.
type SessionFile struct {
	Path    string
	Project string
	ModTime time.Time
	Size    int64
}

// Discover lists transcript files. If project is non-empty, only that
// project folder is scanned. Results are sorted newest first.
func Discover(project string) ([]SessionFile, error) {
	root := ProjectsDir()
	var dirs []string
	if project != "" {
		dirs = []string{filepath.Join(root, project)}
	} else {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	var out []SessionFile
	for _, d := range dirs {
		matches, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			out = append(out, SessionFile{Path: m, Project: filepath.Base(d), ModTime: fi.ModTime(), Size: fi.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}
