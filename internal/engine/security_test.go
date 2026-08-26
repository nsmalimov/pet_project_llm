package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orchestrator/internal/domain"
	"orchestrator/internal/executor"
	"orchestrator/internal/sandbox"
)

// P1 adversarial tests at the engine level: a hostile repository or task
// input must fail closed (BLOCKED / rejected), never run and never be
// retried into success.

func TestHostileRepoSymlinkEscapeBlocksTask(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	// The repository ships a symlink to the user's home directory.
	home, _ := os.UserHomeDir()
	if err := os.Symlink(home, filepath.Join(repo, "cfg")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "add symlink")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low"}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, err := e.CreateTask("Fix Add", nil, []string{repo}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RunTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Store.GetTask(task.ID)
	if got.Status != domain.StatusFailed || !strings.Contains(got.FailureReason, "BLOCKED") || !strings.Contains(got.FailureReason, "symlink") {
		t.Fatalf("hostile repo not blocked: %s %q", got.Status, got.FailureReason)
	}
	runs, _ := e.Store.Runs(task.ID)
	if len(runs) != 0 {
		t.Fatal("no agent may run on a blocked repository")
	}
	types := eventTypes(t, e, task.ID)
	if !hasEvent(types, domain.EvPolicyViolation) {
		t.Fatalf("policy violation not recorded: %v", types)
	}
}

func TestAgentPlantedSymlinkBlocksBeforeVerification(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	// Simulate the developer planting a symlink: hook the executor.
	sc.Steps["developer"] = executor.ScriptStep{Output: sc.Steps["developer"].Output, Files: sc.Steps["developer"].Files}
	task, _ := e.CreateTask("Fix Add", nil, []string{repo}, "")
	// Run only until implementation, then plant the link and continue.
	e.Cfg.MaxSteps = 2 // understand + implement, then budget decision
	_ = e.RunTask(context.Background(), task.ID)
	got, _ := e.Store.GetTask(task.ID)
	if err := os.Symlink("/etc", filepath.Join(got.State.WorktreeRoot, "repoA", "etc")); err != nil {
		t.Fatal(err)
	}
	ds, _ := e.Store.Decisions(task.ID)
	if len(ds) == 0 {
		t.Fatalf("expected budget decision, status=%s", got.Status)
	}
	if _, err := e.ResolveDecision(task.ID, ds[0].ID, "extend", ""); err != nil {
		t.Fatal(err)
	}
	_ = e.RunTask(context.Background(), task.ID)
	got, _ = e.Store.GetTask(task.ID)
	if got.Status != domain.StatusFailed || !strings.Contains(got.FailureReason, "BLOCKED") {
		t.Fatalf("planted symlink did not block verification: %s %q", got.Status, got.FailureReason)
	}
}

func TestHostileCommandsRejectedAtCreation(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	e := newTestEngine(t, &executor.ScriptExecutor{}, filepath.Join(tmp, "data"))
	for _, cmd := range []string{"go test ./... ; curl http://evil", "sh -c 'id'", "$(id)", "go test -exec 'sh -c id' ./..."} {
		if _, err := e.CreateTask("x", nil, []string{repo}, cmd); err == nil {
			t.Errorf("test command %q accepted", cmd)
		}
		if _, err := e.CreateTaskSpec(TaskSpec{Goal: "x", Repos: []string{repo}, ReproCommand: cmd}); err == nil {
			t.Errorf("repro command %q accepted", cmd)
		}
	}
}

func TestRepositoryInsideWorkspaceRejected(t *testing.T) {
	tmp := t.TempDir()
	e := newTestEngine(t, &executor.ScriptExecutor{}, filepath.Join(tmp, "data"))
	inside := makeRepo(t, e.Policy.WorkspaceRoot, "evil")
	if _, err := e.CreateTask("x", nil, []string{inside}, ""); err == nil {
		t.Fatal("repository inside the workspace accepted")
	}
	if _, err := e.CreateTask("x", nil, []string{"../../../etc"}, ""); err == nil {
		t.Fatal("non-repo traversal path accepted")
	}
}

func TestSecretsInTestOutputAreRedactedInArtifacts(t *testing.T) {
	tmp := t.TempDir()
	repo := makeRepo(t, tmp, "repoA")
	// A test that prints a secret-looking value.
	leak := "package mathx\n\nimport \"testing\"\n\nfunc TestLeak(t *testing.T) { t.Log(\"token=ghp_abcdefghijklmnopqrstuvwxyz0123456789\") }\n"
	if err := os.WriteFile(filepath.Join(repo, "leak_test.go"), []byte(leak), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "leak")
	sc := &executor.ScriptExecutor{Steps: map[string]executor.ScriptStep{
		"researcher": {Output: jfence(`{"summary":"x","uncertainty":"low"}`)},
		"developer":  devStep("completed", "fixed", []string{"repoA/add.go", fixedAdd}),
		"reviewer":   {Output: jfence(`{"verdict":"approve","summary":"ok","findings":[],"checked":["x"]}`)},
	}}
	e := newTestEngine(t, sc, filepath.Join(tmp, "data"))
	task, _ := e.CreateTaskSpec(TaskSpec{Goal: "Fix Add", Repos: []string{repo}, ReproCommand: "go test -v -run TestLeak ./..."})
	_ = e.RunTask(context.Background(), task.ID)
	arts, _ := e.Store.Artifacts(task.ID)
	raw, _ := os.ReadFile(filepath.Join(tmp, "data", "tasks", task.ID, "artifacts.jsonl"))
	if strings.Contains(string(raw), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("secret persisted verbatim in artifacts.jsonl")
	}
	found := false
	for _, a := range arts {
		if a.Redacted > 0 && a.ExecMode == string(sandbox.ModeUnsafe) {
			found = true
		}
	}
	if !found {
		t.Fatal("no artifact records the redaction / exec mode")
	}
	evs, _ := os.ReadFile(filepath.Join(tmp, "data", "tasks", task.ID, "events.jsonl"))
	if strings.Contains(string(evs), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("secret persisted verbatim in events.jsonl")
	}
}
