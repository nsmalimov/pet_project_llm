// hindsight — retrospectives for your AI coding-agent sessions.
//
// It reads Claude Code transcripts (~/.claude/projects/**/*.jsonl), shows
// where sessions burned time (error loops, interruptions, denials), and
// distills user corrections into CLAUDE.md rules so the agent stops
// repeating the same mistakes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"hindsight/internal/analyze"
	"hindsight/internal/distill"
	"hindsight/internal/session"
)

const usage = `hindsight — learn from your AI coding-agent sessions

Usage:
  hindsight list                     list recent sessions across all projects
  hindsight report [session]        friction report (no LLM, instant)
  hindsight distill [session]       LLM retrospective: proposed CLAUDE.md rules
  hindsight distill --write [...]   ...and append accepted rules to CLAUDE.md

[session] is a path to a .jsonl transcript. If omitted, the latest session
of the current directory's project is used (excluding the live session that
hindsight itself runs in, if detectable).

Flags for distill:
  --write         append proposed rules to the project's CLAUDE.md
  --model NAME    claude model to use (default: haiku)
  --dir PATH      project dir for CLAUDE.md (default: session's cwd)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "list":
		err = cmdList()
	case "report":
		err = cmdReport(os.Args[2:])
	case "distill":
		err = cmdDistill(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdList() error {
	files, err := session.Discover("")
	if err != nil {
		return fmt.Errorf("no transcripts found in %s: %w", session.ProjectsDir(), err)
	}
	if len(files) == 0 {
		fmt.Println("No Claude Code sessions found.")
		return nil
	}
	fmt.Printf("%-19s  %9s  %s\n", "MODIFIED", "SIZE", "SESSION")
	for _, f := range files {
		fmt.Printf("%-19s  %8.1fK  %s\n", f.ModTime.Format("2006-01-02 15:04:05"), float64(f.Size)/1024, f.Path)
	}
	return nil
}

// resolveSession picks an explicit path or the latest transcript of the
// current directory's project.
func resolveSession(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	proj := session.ProjectDirFor(cwd)
	files, err := session.Discover(proj)
	if err != nil || len(files) == 0 {
		all, aerr := session.Discover("")
		if aerr != nil || len(all) == 0 {
			return "", fmt.Errorf("no sessions found for project %s (and none anywhere else); pass a .jsonl path explicitly", proj)
		}
		fmt.Fprintf(os.Stderr, "note: no sessions for current project, using latest overall\n")
		return all[0].Path, nil
	}
	return files[0].Path, nil
}

func loadAndAnalyze(args []string) (*session.Session, *analyze.Report, error) {
	path, err := resolveSession(args)
	if err != nil {
		return nil, nil, err
	}
	s, err := session.Load(path)
	if err != nil {
		return nil, nil, err
	}
	return s, analyze.Analyze(s), nil
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, r, err := loadAndAnalyze(fs.Args())
	if err != nil {
		return err
	}
	fmt.Print(analyze.Render(r))
	return nil
}

func cmdDistill(args []string) error {
	fs := flag.NewFlagSet("distill", flag.ExitOnError)
	write := fs.Bool("write", false, "append proposed rules to the project's CLAUDE.md")
	model := fs.String("model", "haiku", "claude model for the retrospective")
	dir := fs.String("dir", "", "project dir for CLAUDE.md (default: session cwd)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, r, err := loadAndAnalyze(fs.Args())
	if err != nil {
		return err
	}
	fmt.Print(analyze.Render(r))
	fmt.Fprintf(os.Stderr, "\nRunning retrospective via `claude -p --model %s` (this takes ~30s)...\n\n", *model)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := distill.Distill(ctx, s, r, *model)
	if err != nil {
		return err
	}
	fmt.Print(distill.Render(res))

	if *write && len(res.Rules) > 0 {
		target := *dir
		if target == "" {
			target = s.CWD
		}
		if target == "" {
			return fmt.Errorf("session has no cwd recorded; pass --dir")
		}
		n, path, err := distill.WriteRules(target, s.ID, res.Rules)
		if err != nil {
			return err
		}
		if n == 0 {
			fmt.Printf("\nAll proposed rules already present in %s.\n", path)
		} else {
			fmt.Printf("\nWrote %d rule(s) to %s\n", n, path)
		}
	}
	return nil
}
