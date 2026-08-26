package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"orchestrator/internal/domain"
	"orchestrator/internal/gitws"
	"orchestrator/internal/proof"
	"orchestrator/internal/verify"
)

// This file holds the Proofline additions to the engine: baseline
// reproduction, artifact capture, packet snapshots and human verdicts. The
// workflow itself (steps.go) is unchanged apart from calling into these.

// TaskSpec is the full creation request.
type TaskSpec struct {
	Goal         string
	Context      []string
	Repos        []string
	TestCommand  string
	ReproCommand string
	Kind         domain.TaskKind // empty → inferred from the goal
}

var bugfixWords = regexp.MustCompile(`(?i)\b(fix|bug|regress|broken|crash|incorrect|wrong|duplicate|fails?|leak|race)\b`)

// InferKind guesses bugfix vs change from the goal text. Explicit --kind wins.
func InferKind(goal string) domain.TaskKind {
	if bugfixWords.MatchString(goal) {
		return domain.KindBugfix
	}
	return domain.KindChange
}

// CreateTaskSpec is CreateTask with the Proofline fields.
func (e *Engine) CreateTaskSpec(spec TaskSpec) (*domain.Task, error) {
	switch spec.Kind {
	case "", domain.KindBugfix, domain.KindChange:
	default:
		return nil, fmt.Errorf("unknown task kind %q (bugfix|change)", spec.Kind)
	}
	t, err := e.CreateTask(spec.Goal, spec.Context, spec.Repos, spec.TestCommand)
	if err != nil {
		return nil, err
	}
	t.ReproCommand = strings.TrimSpace(spec.ReproCommand)
	t.Kind = spec.Kind
	if t.Kind == "" {
		t.Kind = InferKind(spec.Goal)
	}
	if err := e.Store.SaveTask(t); err != nil {
		return nil, err
	}
	return t, nil
}

// ---------- artifacts ----------

func (e *Engine) addArtifact(t *domain.Task, a domain.Artifact) domain.Artifact {
	a.ID = domain.NewID("art")
	a.TaskID = t.ID
	a.Phase = t.Status
	a.At = time.Now().UTC()
	if err := e.Store.AddArtifact(a); err != nil {
		e.emit(t.ID, domain.EvWarning, map[string]any{"warning": "artifact not persisted", "error": err.Error(), "kind": string(a.Kind)})
		return a
	}
	e.emit(t.ID, domain.EvArtifactAdded, map[string]any{"artifact_id": a.ID, "kind": string(a.Kind), "title": a.Title})
	return a
}

func boolp(b bool) *bool { return &b }

// verificationCommands returns the (command, narrow) pairs to run in a repo:
// the repro command first when it differs from the test command.
type verifyCmd struct {
	Cmd    string
	Narrow bool
}

func verificationCommands(t *domain.Task, dir string) []verifyCmd {
	type pair = verifyCmd
	var out []pair
	test := t.TestCommand
	if test == "" {
		test = verify.DetectCommand(dir)
	}
	if t.ReproCommand != "" && t.ReproCommand != test {
		out = append(out, pair{t.ReproCommand, true})
	}
	if test != "" {
		out = append(out, pair{test, false})
	}
	return out
}

// runBaseline executes the repro/test commands on the untouched worktrees
// exactly once per task and records the outcome as artifacts. For bugfix
// tasks a failing baseline is the reproduction; a passing one is recorded as
// a contradiction (the test does not exercise the bug) — the packet decides.
func (e *Engine) runBaseline(ctx context.Context, t *domain.Task) error {
	if t.State.BaselineDone {
		return nil
	}
	var results []domain.TestResult
	ran := 0
	for _, r := range t.Repos {
		dir := gitws.RepoDir(t, r)
		for _, c := range verificationCommands(t, dir) {
			ran++
			e.emit(t.ID, domain.EvBaselineStarted, map[string]any{"repo": r.Name, "command": c.Cmd, "narrow": c.Narrow})
			res := verify.Run(ctx, r.Name, dir, c.Cmd, e.Cfg.TestTimeout)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			results = append(results, res)
			title := "baseline: " + c.Cmd
			e.addArtifact(t, domain.Artifact{
				Kind: domain.ArtBaselineRun, Title: title, Repo: r.Name, Command: c.Cmd,
				ExitCode: res.ExitCode, Passed: boolp(res.Passed), Output: res.Output, Narrow: c.Narrow,
			})
			if res.Passed {
				e.emit(t.ID, domain.EvBaselinePassed, map[string]any{"repo": r.Name, "command": c.Cmd})
			} else {
				e.emit(t.ID, domain.EvBaselineFailed, map[string]any{
					"repo": r.Name, "command": c.Cmd, "exit_code": res.ExitCode, "output_tail": tailStr(res.OutputTail, 1500),
				})
			}
		}
	}
	t.State.Baseline = results
	t.State.BaselineDone = true
	if ran == 0 {
		e.emit(t.ID, domain.EvBaselineSkipped, map[string]any{"reason": "no test or repro command detected in any repo"})
	} else if t.Kind == domain.KindBugfix && len(failedOnly(results)) > 0 {
		e.addEvidence(t, domain.EvidenceReproduced,
			"the reported failure reproduces on the unchanged code", "tester", summarizeTests(results))
	}
	return e.Store.SaveTask(t)
}

// ---------- packets & verdicts ----------

// BuildPacket derives the packet from persisted state and appends it as a
// new version when its content changed. Safe to call at any time.
func (e *Engine) BuildPacket(taskID string) (*domain.Packet, error) {
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	return e.snapshotPacket(t)
}

func (e *Engine) snapshotPacket(t *domain.Task) (*domain.Packet, error) {
	arts, err := e.Store.Artifacts(t.ID)
	if err != nil {
		return nil, err
	}
	evidence, _ := e.Store.EvidenceList(t.ID)
	runs, _ := e.Store.Runs(t.ID)
	decisions, _ := e.Store.Decisions(t.ID)
	fresh := proof.Build(proof.Input{
		Task: t, Artifacts: arts, Evidence: evidence, Runs: runs, Decisions: decisions,
		FileExists: proof.WorktreeFileExists(t),
	})
	existing, err := e.Store.Packets(t.ID)
	if err != nil {
		return nil, err
	}
	p, isNew := proof.NextVersion(existing, fresh)
	if isNew {
		if err := e.Store.AddPacket(p); err != nil {
			return nil, err
		}
		e.emit(t.ID, domain.EvPacketBuilt, map[string]any{
			"version": p.Version, "verdict": string(p.Verdict), "gaps": len(p.Gaps), "fingerprint": p.Fingerprint,
		})
	}
	return &p, nil
}

// PacketView is what the Change Case screen renders.
type PacketView struct {
	Task      *domain.Task      `json:"task"`
	Packet    *domain.Packet    `json:"packet"`
	Versions  []packetVersion   `json:"versions"`
	Artifacts []domain.Artifact `json:"artifacts"`
	Verdicts  []domain.Verdict  `json:"verdicts"`
	Runs      []domain.AgentRun `json:"runs"`
	Evidence  []domain.Evidence `json:"evidence"`
	Decisions []domain.Decision `json:"decisions"`
	Totals    Totals            `json:"totals"`
}

type packetVersion struct {
	Version     int                `json:"version"`
	BuiltAt     time.Time          `json:"built_at"`
	Verdict     domain.ClaimStatus `json:"verdict"`
	TaskStatus  domain.TaskStatus  `json:"task_status"`
	Fingerprint string             `json:"fingerprint"`
}

// PacketState returns the latest packet (building one if the task changed)
// plus everything needed to drill down to raw evidence.
func (e *Engine) PacketState(taskID string) (*PacketView, error) {
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	p, err := e.snapshotPacket(t)
	if err != nil {
		return nil, err
	}
	fs, err := e.FullState(taskID)
	if err != nil {
		return nil, err
	}
	arts, _ := e.Store.Artifacts(taskID)
	verdicts, _ := e.Store.Verdicts(taskID)
	all, _ := e.Store.Packets(taskID)
	v := &PacketView{Task: t, Packet: p, Artifacts: arts, Verdicts: verdicts,
		Runs: fs.Runs, Evidence: fs.Evidence, Decisions: fs.Decisions, Totals: fs.Totals}
	for _, x := range all {
		v.Versions = append(v.Versions, packetVersion{x.Version, x.BuiltAt, x.Verdict, x.TaskStatus, x.Fingerprint})
	}
	if v.Artifacts == nil {
		v.Artifacts = []domain.Artifact{}
	}
	if v.Verdicts == nil {
		v.Verdicts = []domain.Verdict{}
	}
	if v.Versions == nil {
		v.Versions = []packetVersion{}
	}
	if v.Runs == nil {
		v.Runs = []domain.AgentRun{}
	}
	if v.Evidence == nil {
		v.Evidence = []domain.Evidence{}
	}
	if v.Decisions == nil {
		v.Decisions = []domain.Decision{}
	}
	return v, nil
}

// PacketVersion returns one historical packet version.
func (e *Engine) PacketVersion(taskID string, version int) (*domain.Packet, error) {
	all, err := e.Store.Packets(taskID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Version == version {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("packet version %d not found", version)
}

var ErrTaskRunning = errors.New("task is still running; a verdict needs a finished packet")

// RecordVerdict stores the human merge decision against the current packet
// version. It is refused while the workflow is still executing, so a verdict
// always refers to a packet whose content the human could actually see.
func (e *Engine) RecordVerdict(taskID, decision, note, by string) (*domain.Verdict, error) {
	switch decision {
	case "accept", "request_changes", "reject":
	default:
		return nil, fmt.Errorf("unknown decision %q (accept|request_changes|reject)", decision)
	}
	t, err := e.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if t.Status.Active() {
		return nil, ErrTaskRunning
	}
	p, err := e.snapshotPacket(t)
	if err != nil {
		return nil, err
	}
	v := domain.Verdict{
		ID: domain.NewID("ver"), TaskID: taskID, PacketVersion: p.Version,
		Decision: decision, Note: strings.TrimSpace(note), By: by, At: time.Now().UTC(),
	}
	if err := e.Store.AddVerdict(v); err != nil {
		return nil, err
	}
	e.emit(taskID, domain.EvVerdictRecorded, map[string]any{
		"verdict_id": v.ID, "decision": decision, "packet_version": p.Version, "note": v.Note, "by": by,
	})
	return &v, nil
}
