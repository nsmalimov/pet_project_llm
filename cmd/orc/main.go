// Command orc is the orchestrator CLI: create and run tasks, serve the HTTP
// API, inspect state, resolve decisions, manage memory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"orchestrator/examples"
	"orchestrator/internal/api"
	"orchestrator/internal/auth"
	"orchestrator/internal/domain"
	"orchestrator/internal/engine"
	"orchestrator/internal/executor"
	"orchestrator/internal/github"
	"orchestrator/internal/gitws"
	"orchestrator/internal/memory"
	"orchestrator/internal/repos"
	"orchestrator/internal/router"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "create":
		err = cmdCreate(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "show":
		err = cmdShow(os.Args[2:])
	case "events":
		err = cmdEvents(os.Args[2:])
	case "resolve":
		err = cmdResolve(os.Args[2:])
	case "resume":
		err = cmdResume(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "packet":
		err = cmdPacket(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "github-status":
		err = cmdGithubStatus(os.Args[2:])
	case "decide":
		err = cmdDecide(os.Args[2:])
	case "repo":
		err = cmdRepo(os.Args[2:])
	case "auth":
		err = cmdAuth(os.Args[2:])
	case "memory":
		err = cmdMemory(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `orc — engineering task orchestrator

Usage:
  orc create --repo <path> [--repo <path>...] --task "<goal>" [flags]
      --context <text>     extra context (repeatable)
      --executor <name>    claude | mock            (default claude)
      --script <file>      scenario file for the mock executor
      --test-cmd <cmd>     override auto-detected test command
      --repro-cmd <cmd>    narrow command that must FAIL before and PASS after the change
      --kind <kind>        bugfix | change (inferred from the goal when empty)
      --no-run             create the task without executing it
  orc serve   [--addr :8080]
  orc list
  orc show    <task-id>
  orc events  <task-id> [--after N]
  orc resolve <task-id> <decision-id> --option <id> [--note "..."]
  orc resume  <task-id>
  orc run     <task-id>        start a task created with --no-run (or continue a stopped one)
  orc verify  --repo <path> --head <ref> --task "<goal>" [--repro-cmd ...] [--pr owner/repo#N]
                                   verify an EXISTING change (PR head/branch/sha): baseline on base,
                                   tests + independent challenge on head, no developer
  orc packet  <task-id> [--json]   change case: verdict, claims, evidence, gaps, risks
  orc github-status <task-id> [--sha <sha>] [--url <packet-url>] [--post]
                                   commit status + PR comment for the packet (never a fake green);
                                   --post needs GITHUB_TOKEN and --pr on the task
  orc decide  <task-id> --decision accept|request_changes|reject [--note "..."]
  orc repo    add <path> | list       register a source repository (tasks may reference repo IDs;
                                     SAFE_SANDBOX accepts IDs only)
  orc auth    init --workspace <name> --user <name>       first owner; prints a token once
  orc auth    add-user --workspace <ws-id> --user <name> --role owner|admin|member|reviewer|viewer
  orc auth    revoke --workspace <ws-id> --user <user-id>
  orc memory  add --kind <preference|project_rule|correction> [--scope name] "<text>"
  orc memory  list

Common flags:
  --data <dir>   data directory (default ./.orchestrator)

Environment:
  PROOFLINE_SANDBOX=SAFE_SANDBOX|LOCAL_UNSAFE   execution boundary (default LOCAL_UNSAFE, printed loudly)
  PROOFLINE_REPOS_ROOT=<dir>[:<dir>]            only repositories under these roots may be used
  GITHUB_TOKEN, PROOFLINE_GITHUB_WEBHOOK_SECRET, PROOFLINE_PUBLIC_URL, PROOFLINE_GITHUB_API   GitHub integration

`)
}

// reorderArgs moves flags (and their values) in front of positional args so
// `orc show <id> --data X` works; Go's flag package stops at the first
// positional argument otherwise. boolFlags take no value.
func reorderArgs(args []string, boolFlags ...string) []string {
	isBool := map[string]bool{}
	for _, b := range boolFlags {
		isBool[b] = true
	}
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(a, "=") && !isBool[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

// stringSlice is a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

type app struct {
	eng *engine.Engine
	mem *memory.FileStore
}

func buildApp(dataDir, execName, scriptPath string) (*app, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	st, err := store.NewFileStore(abs)
	if err != nil {
		return nil, err
	}
	pol, err := sandbox.Default(abs)
	if err != nil {
		return nil, err
	}
	if m := os.Getenv("PROOFLINE_SANDBOX"); m != "" {
		pol, err = pol.WithMode(sandbox.Mode(m))
		if err != nil {
			return nil, err
		}
	}
	if roots := os.Getenv("PROOFLINE_REPOS_ROOT"); roots != "" {
		for _, r := range strings.Split(roots, ":") {
			c, err := sandbox.Canonical(r)
			if err != nil {
				return nil, fmt.Errorf("PROOFLINE_REPOS_ROOT: %w", err)
			}
			pol.ReposRoots = append(pol.ReposRoots, c)
		}
	}
	if w := pol.Warning(); w != "" {
		fmt.Fprintln(os.Stderr, "⚠ "+w+" (set PROOFLINE_SANDBOX=SAFE_SANDBOX and PROOFLINE_REPOS_ROOT=<dir> to enforce the boundary)")
	}
	mem, err := memory.NewFileStore(abs)
	if err != nil {
		return nil, err
	}
	claude := executor.NewClaudeCLI()
	claude.Policy = &pol
	execs := map[string]executor.Executor{
		"claude":   claude,
		"scenario": &executor.ScenarioExecutor{Lookup: func(n string) (*executor.ScriptExecutor, error) { sc, _, err := examples.Load(n); return sc, err }},
	}
	if scriptPath != "" {
		sc, err := executor.LoadScript(scriptPath)
		if err != nil {
			return nil, err
		}
		execs["mock"] = sc
		if execName == "" {
			execName = "mock"
		}
	}
	if execName == "" {
		execName = "claude"
	}
	if _, ok := execs[execName]; !ok {
		return nil, fmt.Errorf("executor %q not available (did you forget --script for mock?)", execName)
	}
	rt := router.Rules{Executor: execName, CheapModel: "sonnet", StrongModel: "opus"}
	ws := gitws.NewManager(filepath.Join(abs, "worktrees"))
	eng := engine.New(st, ws, execs, rt, mem, engine.DefaultConfig())
	eng.SetPolicy(pol)
	eng.Repos = repos.Open(abs, pol)
	return &app{eng: eng, mem: mem}, nil
}

func printEvent(ev domain.Event) {
	data := ""
	if len(ev.Data) > 0 {
		b, _ := json.Marshal(ev.Data)
		data = string(b)
		if len(data) > 220 {
			data = data[:220] + "…"
		}
	}
	fmt.Printf("%s  #%-3d %-22s %s\n", ev.At.Local().Format("15:04:05"), ev.Seq, ev.Type, data)
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	var repos, ctxSrcs stringSlice
	fs.Var(&repos, "repo", "repository path (repeatable)")
	fs.Var(&ctxSrcs, "context", "extra context (repeatable)")
	task := fs.String("task", "", "engineering task description")
	dataDir := fs.String("data", ".orchestrator", "data directory")
	execName := fs.String("executor", "", "executor: claude | mock")
	script := fs.String("script", "", "scenario file for mock executor")
	testCmd := fs.String("test-cmd", "", "override test command")
	reproCmd := fs.String("repro-cmd", "", "narrow command expected to fail before the change and pass after (e.g. 'go test -run TestX ./...')")
	kind := fs.String("kind", "", "task kind: bugfix | change (inferred from the goal when empty)")
	noRun := fs.Bool("no-run", false, "create without running")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := buildApp(*dataDir, *execName, *script)
	if err != nil {
		return err
	}
	a.eng.OnEvent = printEvent

	t, err := a.eng.CreateTaskSpec(engine.TaskSpec{
		Goal: *task, Context: ctxSrcs, Repos: repos, TestCommand: *testCmd,
		ReproCommand: *reproCmd, Kind: domain.TaskKind(*kind),
	})
	if err != nil {
		return err
	}
	fmt.Printf("created task %s (kind=%s)\n", t.ID, t.Kind)
	if *noRun {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.eng.RunTask(ctx, t.ID); err != nil {
		return err
	}
	return printState(a.eng, t.ID)
}

func printState(eng *engine.Engine, id string) error {
	fs, err := eng.FullState(id)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("task %s: %s\n", fs.Task.ID, fs.Task.Status)
	if fs.Task.FailureReason != "" {
		fmt.Printf("failure: %s\n", fs.Task.FailureReason)
	}
	fmt.Printf("confidence: %s\n", fs.Confidence)
	if len(fs.Task.State.ChangedFiles) > 0 {
		fmt.Printf("changed files: %s\n", strings.Join(fs.Task.State.ChangedFiles, ", "))
		fmt.Printf("worktrees: %s (branch %s)\n", fs.Task.State.WorktreeRoot, fs.Task.State.Branch)
	}
	fmt.Printf("agent runs: %d, tokens in/out: %d/%d, cost: $%.4f\n",
		fs.Totals.AgentRuns, fs.Totals.InputTokens, fs.Totals.OutputTokens, fs.Totals.CostUSD)
	for _, d := range fs.Decisions {
		if d.Status == "open" {
			fmt.Printf("\nOPEN DECISION %s [%s]: %s\n", d.ID, d.Importance, d.Question)
			if d.Recommendation != "" {
				fmt.Printf("  recommendation: %s\n", d.Recommendation)
			}
			for _, o := range d.Options {
				fmt.Printf("  --option %-10s %s\n", o.ID, o.Label)
			}
			fmt.Printf("resolve with: orc resolve %s %s --option <id>\n", fs.Task.ID, d.ID)
		}
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dataDir := fs.String("data", ".orchestrator", "data directory")
	execName := fs.String("executor", "claude", "executor for LLM roles")
	script := fs.String("script", "", "scenario file for mock executor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := buildApp(*dataDir, *execName, *script)
	if err != nil {
		return err
	}
	if err := a.eng.RecoverInterrupted(); err != nil {
		return err
	}
	srv := api.New(a.eng)
	as, err := auth.Open(*dataDir)
	if err != nil {
		return err
	}
	srv.Auth = as
	if !as.Configured() && !isLoopback(*addr) {
		return fmt.Errorf("refusing to bind %s without authentication configured (run `orc auth init` or bind to 127.0.0.1)", *addr)
	}
	abs, _ := filepath.Abs(*dataDir)
	srv.ExampleRoot = filepath.Join(filepath.Dir(abs), filepath.Base(abs)+"-examples")
	if v := os.Getenv("PROOFLINE_EXAMPLES"); v == "off" {
		srv.ExampleRoot = ""
	}
	srv.WebhookSecret = os.Getenv("PROOFLINE_GITHUB_WEBHOOK_SECRET")
	srv.PublicURL = strings.TrimRight(os.Getenv("PROOFLINE_PUBLIC_URL"), "/")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		srv.GitHub = &github.Client{Token: tok, BaseURL: os.Getenv("PROOFLINE_GITHUB_API")}
	}
	fmt.Printf("orchestrator API on %s (data: %s, sandbox: %s, github: %v)\n", *addr, *dataDir, a.eng.Policy.Mode, srv.GitHub != nil)
	return http.ListenAndServe(*addr, srv.Handler())
}

func openApp(fs *flag.FlagSet, args []string) (*app, []string, error) {
	dataDir := fs.String("data", ".orchestrator", "data directory")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	a, err := buildApp(*dataDir, "claude", "")
	return a, fs.Args(), err
}

func cmdShow(args []string) error {
	args = reorderArgs(args)
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	a, rest, err := openApp(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: orc show <task-id>")
	}
	state, err := a.eng.FullState(rest[0])
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(state, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdEvents(args []string) error {
	args = reorderArgs(args)
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	after := fs.Int64("after", 0, "only events after this seq")
	dataDir := fs.String("data", ".orchestrator", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: orc events <task-id>")
	}
	a, err := buildApp(*dataDir, "claude", "")
	if err != nil {
		return err
	}
	evs, err := a.eng.Store.Events(fs.Arg(0), *after)
	if err != nil {
		return err
	}
	for _, ev := range evs {
		printEvent(ev)
	}
	return nil
}

func cmdResolve(args []string) error {
	args = reorderArgs(args)
	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	option := fs.String("option", "", "chosen option id")
	note := fs.String("note", "", "extra guidance")
	dataDir := fs.String("data", ".orchestrator", "data directory")
	executorName := fs.String("executor", "claude", "executor for continued execution")
	script := fs.String("script", "", "scenario file for mock executor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: orc resolve <task-id> <decision-id> --option <id>")
	}
	if *option == "" {
		return errors.New("--option is required")
	}
	a, err := buildApp(*dataDir, *executorName, *script)
	if err != nil {
		return err
	}
	a.eng.OnEvent = printEvent
	t, err := a.eng.ResolveDecision(fs.Arg(0), fs.Arg(1), *option, *note)
	if err != nil {
		return err
	}
	if t.Status.Terminal() {
		return printState(a.eng, t.ID)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.eng.RunTask(ctx, t.ID); err != nil {
		return err
	}
	return printState(a.eng, t.ID)
}

func cmdResume(args []string) error {
	args = reorderArgs(args)
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	dataDir := fs.String("data", ".orchestrator", "data directory")
	executorName := fs.String("executor", "claude", "executor")
	script := fs.String("script", "", "scenario file for mock executor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: orc resume <task-id>")
	}
	a, err := buildApp(*dataDir, *executorName, *script)
	if err != nil {
		return err
	}
	a.eng.OnEvent = printEvent
	if err := a.eng.RecoverInterrupted(); err != nil {
		return err
	}
	t, err := a.eng.Resume(fs.Arg(0))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.eng.RunTask(ctx, t.ID); err != nil {
		return err
	}
	return printState(a.eng, t.ID)
}

func cmdRun(args []string) error {
	args = reorderArgs(args)
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dataDir := fs.String("data", ".orchestrator", "data directory")
	executorName := fs.String("executor", "claude", "executor")
	script := fs.String("script", "", "scenario file for mock executor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: orc run <task-id>")
	}
	a, err := buildApp(*dataDir, *executorName, *script)
	if err != nil {
		return err
	}
	a.eng.OnEvent = printEvent
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.eng.RunTask(ctx, fs.Arg(0)); err != nil {
		return err
	}
	return printState(a.eng, fs.Arg(0))
}

func cmdList(args []string) error {
	args = reorderArgs(args)
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	a, _, err := openApp(fs, args)
	if err != nil {
		return err
	}
	tasks, err := a.eng.Store.ListTasks()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		fmt.Printf("%s  %-18s  %s\n", t.ID, t.Status, t.Goal)
	}
	return nil
}

func cmdMemory(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: orc memory add|list")
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("memory add", flag.ExitOnError)
		kind := fs.String("kind", "preference", "preference | project_rule | correction")
		scope := fs.String("scope", "", "repo name/path scope (empty = global)")
		dataDir := fs.String("data", ".orchestrator", "data directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return errors.New("usage: orc memory add [--kind k] \"text\"")
		}
		a, err := buildApp(*dataDir, "claude", "")
		if err != nil {
			return err
		}
		return a.mem.Add(memory.Item{
			ID: domain.NewID("mem"), Kind: memory.Kind(*kind), Scope: *scope,
			Text: strings.Join(fs.Args(), " "), Status: "confirmed",
		})
	case "list":
		fs := flag.NewFlagSet("memory list", flag.ExitOnError)
		dataDir := fs.String("data", ".orchestrator", "data directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		a, err := buildApp(*dataDir, "claude", "")
		if err != nil {
			return err
		}
		items, err := a.mem.List("")
		if err != nil {
			return err
		}
		for _, it := range items {
			scope := it.Scope
			if scope == "" {
				scope = "global"
			}
			fmt.Printf("%s  %-12s  %-10s  %s\n", it.ID, it.Kind, scope, it.Text)
		}
		return nil
	default:
		return fmt.Errorf("unknown memory subcommand %q", args[0])
	}
}

// cmdPacket prints the change-case packet (claims → evidence → verdict).
func cmdPacket(args []string) error {
	args = reorderArgs(args, "json")
	fs := flag.NewFlagSet("packet", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the full packet view as JSON")
	a, rest, err := openApp(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: orc packet <task-id> [--json]")
	}
	v, err := a.eng.PacketState(rest[0])
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	p := v.Packet
	fmt.Printf("CHANGE CASE %s  %s\n", v.Task.ID, v.Task.Goal)
	fmt.Printf("VERDICT  %s  — %s  (packet v%d, task %s)\n\n", strings.ToUpper(string(p.Verdict)), p.VerdictWhy, p.Version, v.Task.Status)
	fmt.Printf("CHANGE   branch %s  commits %v\n         files: %s\n\n", p.Change.Branch, p.Change.Commits, strings.Join(p.Change.Files, ", "))
	fmt.Println("CLAIMS")
	for _, c := range p.Claims {
		fmt.Printf("  %-13s %-24s %s\n", strings.ToUpper(string(c.Status)), c.Title, c.Statement)
		fmt.Printf("  %-13s %-24s ↳ %s", "", "", c.Reason)
		if len(c.ArtifactIDs) > 0 {
			fmt.Printf("  [%s]", strings.Join(c.ArtifactIDs, " "))
		}
		fmt.Println()
	}
	if len(p.Gaps) > 0 {
		fmt.Println("\nNOT VERIFIED")
		for _, g := range p.Gaps {
			fmt.Println("  ▲ " + g)
		}
	}
	if len(p.Risks) > 0 {
		fmt.Println("\nUNRESOLVED RISKS")
		for _, r := range p.Risks {
			fmt.Printf("  [%s] %s: %s\n", r.Severity, r.Source, r.Text)
		}
	}
	fmt.Println("\nHUMAN DECISION")
	if len(v.Verdicts) == 0 {
		fmt.Println("  none yet — orc decide <task-id> --decision accept|request_changes|reject --note '...'")
	}
	for _, d := range v.Verdicts {
		fmt.Printf("  %s on packet v%d at %s  %s\n", strings.ToUpper(d.Decision), d.PacketVersion, d.At.Local().Format("2006-01-02 15:04"), d.Note)
	}
	return nil
}

// cmdDecide records the human merge decision on the current packet.
func cmdDecide(args []string) error {
	args = reorderArgs(args)
	fs := flag.NewFlagSet("decide", flag.ExitOnError)
	decision := fs.String("decision", "", "accept | request_changes | reject")
	note := fs.String("note", "", "why")
	by := fs.String("by", os.Getenv("USER"), "who decides")
	pv := fs.Int("packet-version", 0, "packet version you reviewed (refused if it changed since)")
	a, rest, err := openApp(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || *decision == "" {
		return errors.New("usage: orc decide <task-id> --decision accept|request_changes|reject [--note ...]")
	}
	v, err := a.eng.RecordVerdict(rest[0], *decision, *note, *by, *pv)
	if err != nil {
		return err
	}
	fmt.Printf("recorded %s on packet v%d (%s)\n", v.Decision, v.PacketVersion, v.ID)
	return nil
}

// parsePR parses "owner/repo#N".
func parsePR(s string) (*domain.PullRequestRef, error) {
	if s == "" {
		return nil, nil
	}
	repoPart, numPart, ok := strings.Cut(s, "#")
	owner, repo, ok2 := strings.Cut(repoPart, "/")
	if !ok || !ok2 || owner == "" || repo == "" {
		return nil, fmt.Errorf("--pr must be owner/repo#N, got %q", s)
	}
	var n int
	if _, err := fmt.Sscanf(numPart, "%d", &n); err != nil || n <= 0 {
		return nil, fmt.Errorf("--pr must be owner/repo#N, got %q", s)
	}
	return &domain.PullRequestRef{Owner: owner, Repo: repo, Number: n, URL: fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)}, nil
}

// cmdVerify creates and runs a verify-only task for an existing change.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var repos, ctxSrcs stringSlice
	fs.Var(&repos, "repo", "repository path (repeatable)")
	fs.Var(&ctxSrcs, "context", "extra context (repeatable)")
	task := fs.String("task", "", "what the change is supposed to do")
	head := fs.String("head", "", "ref of the change to verify (branch, tag, sha); the repo's HEAD is the base")
	pr := fs.String("pr", "", "owner/repo#N to link the case to a pull request")
	dataDir := fs.String("data", ".orchestrator", "data directory")
	execName := fs.String("executor", "", "executor: claude | mock")
	script := fs.String("script", "", "scenario file for mock executor")
	testCmd := fs.String("test-cmd", "", "override test command")
	reproCmd := fs.String("repro-cmd", "", "narrow command expected to fail on base and pass on head")
	kind := fs.String("kind", "", "bugfix | change")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *head == "" {
		return errors.New("--head is required")
	}
	prRef, err := parsePR(*pr)
	if err != nil {
		return err
	}
	a, err := buildApp(*dataDir, *execName, *script)
	if err != nil {
		return err
	}
	a.eng.OnEvent = printEvent
	t, err := a.eng.CreateTaskSpec(engine.TaskSpec{
		Goal: *task, Context: ctxSrcs, Repos: repos, TestCommand: *testCmd, ReproCommand: *reproCmd,
		Kind: domain.TaskKind(*kind), HeadRef: *head, PR: prRef,
	})
	if err != nil {
		return err
	}
	fmt.Printf("created verify-only task %s (kind=%s, head=%s)\n", t.ID, t.Kind, t.HeadRef)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.eng.RunTask(ctx, t.ID); err != nil {
		return err
	}
	return printState(a.eng, t.ID)
}

// cmdGithubStatus prints (and optionally posts) the commit status and PR
// comment derived from the packet. Without --post nothing leaves the machine.
func cmdGithubStatus(args []string) error {
	args = reorderArgs(args, "post")
	fs := flag.NewFlagSet("github-status", flag.ExitOnError)
	sha := fs.String("sha", "", "commit to report on (default: the packet's head SHA)")
	url := fs.String("url", "", "packet URL to link (default http://127.0.0.1:8080/cases/<id>)")
	post := fs.Bool("post", false, "POST to GitHub (needs GITHUB_TOKEN and a --pr on the task)")
	a, rest, err := openApp(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: orc github-status <task-id> [--sha ..] [--post]")
	}
	v, err := a.eng.PacketState(rest[0])
	if err != nil {
		return err
	}
	repoName := v.Task.Repos[0].Name
	target := *sha
	if target == "" {
		target = v.Packet.Source.HeadSHAs[repoName]
	}
	link := *url
	if link == "" {
		link = "http://127.0.0.1:8080/cases/" + v.Task.ID
	}
	st := github.BuildStatus(v.Packet, repoName, target, link)
	comment := github.BuildComment(v.Packet, repoName, target, link)
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Printf("commit status for %s:\n%s\n\nPR comment:\n%s\n", target, b, comment)
	if !*post {
		return nil
	}
	if v.Task.PR == nil {
		return errors.New("task has no --pr link; cannot post")
	}
	c := &github.Client{Token: os.Getenv("GITHUB_TOKEN")}
	ctx := context.Background()
	if err := c.PostStatus(ctx, v.Task.PR.Owner, v.Task.PR.Repo, target, st); err != nil {
		return err
	}
	if err := c.PostComment(ctx, v.Task.PR.Owner, v.Task.PR.Repo, v.Task.PR.Number, comment); err != nil {
		return err
	}
	fmt.Println("posted")
	return nil
}

func cmdRepo(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: orc repo add <path> | orc repo list")
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("repo add", flag.ExitOnError)
		gh := fs.String("github", "", "owner/name this clone mirrors (enables webhook/PR import)")
		a, rest, err := openApp(fs, reorderArgs(args[1:]))
		if err != nil {
			return err
		}
		if len(rest) < 1 {
			return errors.New("usage: orc repo add <path>")
		}
		rp, err := a.eng.Repos.Add(rest[0], "")
		if err != nil {
			return err
		}
		if *gh != "" {
			if err := a.eng.Repos.SetGitHub(rp.ID, *gh); err != nil {
				return err
			}
		}
		fmt.Printf("%s  %s  %s  %s\n", rp.ID, rp.Name, rp.Path, *gh)
		return nil
	case "list":
		fs := flag.NewFlagSet("repo list", flag.ExitOnError)
		a, _, err := openApp(fs, args[1:])
		if err != nil {
			return err
		}
		list, err := a.eng.Repos.List()
		if err != nil {
			return err
		}
		for _, rp := range list {
			fmt.Printf("%s  %s  %s\n", rp.ID, rp.Name, rp.Path)
		}
		return nil
	}
	return fmt.Errorf("unknown repo subcommand %q", args[0])
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cmdAuth(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: orc auth init|add-user|revoke ...")
	}
	sub := args[0]
	fs := flag.NewFlagSet("auth "+sub, flag.ExitOnError)
	dataDir := fs.String("data", ".orchestrator", "data directory")
	wsName := fs.String("workspace", "", "workspace name (init) or id (add-user/revoke)")
	user := fs.String("user", "", "user name (init/add-user) or id (revoke)")
	role := fs.String("role", "member", "role for add-user")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	as, err := auth.Open(*dataDir)
	if err != nil {
		return err
	}
	switch sub {
	case "init":
		if *wsName == "" || *user == "" {
			return errors.New("--workspace and --user are required")
		}
		ws, err := as.CreateWorkspace(*wsName)
		if err != nil {
			return err
		}
		u, err := as.CreateUser(*user)
		if err != nil {
			return err
		}
		if err := as.SetMembership(u.ID, ws.ID, auth.RoleOwner); err != nil {
			return err
		}
		tok, err := as.IssueToken(u.ID, "initial")
		if err != nil {
			return err
		}
		fmt.Printf("workspace %s (%s)\nuser %s (%s) role owner\ntoken (shown once): %s\n", ws.ID, ws.Name, u.ID, u.Name, tok)
		return nil
	case "add-user":
		if *wsName == "" || *user == "" {
			return errors.New("--workspace <ws-id> and --user <name> are required")
		}
		u, err := as.CreateUser(*user)
		if err != nil {
			return err
		}
		if err := as.SetMembership(u.ID, *wsName, auth.Role(*role)); err != nil {
			return err
		}
		tok, err := as.IssueToken(u.ID, "initial")
		if err != nil {
			return err
		}
		fmt.Printf("user %s (%s) role %s in %s\ntoken (shown once): %s\n", u.ID, u.Name, *role, *wsName, tok)
		return nil
	case "revoke":
		if *wsName == "" || *user == "" {
			return errors.New("--workspace <ws-id> and --user <user-id> are required")
		}
		return as.RevokeMembership(*user, *wsName)
	}
	return fmt.Errorf("unknown auth subcommand %q", sub)
}
