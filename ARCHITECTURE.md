# Architecture

A modular monolith. One process, one binary, packages with strict
dependency direction:

```
cmd/orc ──► api ──► engine ──► roles, router, gitws, verify, executor, store, memory
                                                │
                     all packages depend on ──► domain (pure types, no deps)
```

## Decisions and their rationale

**Stdlib only, file-based persistence.** The prototype must run anywhere and
its state must be debuggable with `cat`. Every task is a directory:
`task.json` (snapshot), `events.jsonl` (append-only log), `runs.jsonl`,
`evidence.jsonl`, `decisions.json`. The `store.Store` interface is the seam —
SQLite/Postgres later is a new implementation, zero changes elsewhere.

**Events + snapshot, not full event sourcing.** Events are the observable
truth for UIs (append-only, seq-numbered, poll with `?after=N`). But the
engine reads/writes the task snapshot (`TaskState`) rather than replaying
events — replay machinery would be framework-building the product doesn't
need yet. The event log is still complete enough to rebuild state later if
that ever becomes worth it.

**The workflow is a re-planner, not a pipeline.** `engine.RunTask` is a loop:
load persisted task → decide the next step from its status and state →
execute → persist → repeat. Branches are data-driven: researcher uncertainty
spawns investigation; developer `blocked` spawns investigation or a decision;
failing tests loop back to implementation with the failure output; review
findings loop back too. Counters (`FixAttempts`, `ReviewRounds`,
`Investigations`, `Steps`) bound every loop and escalate to a structured
human decision instead of failing silently or looping forever.

**Roles are contracts, not personas.** Each role
(`internal/roles`) has its own goal, prompt, context slice, output schema and
permissions:

| Role | Sees | Can write | Output |
| --- | --- | --- | --- |
| Researcher | goal, repos, memory rules | no | summary, key files, uncertainty, open questions, decision_request |
| Developer | goal, research findings, test failures, review findings, memory | yes | status (completed/blocked/uncertain), summary, files, decision_request |
| Reviewer | goal, **diff**, read-only worktree, memory — **never the developer's reasoning** | no | verdict, findings |
| Tester | the worktree | no (runs commands) | real test results |

Reviewer independence is enforced twice: by context (no developer output in
the prompt — there's a test asserting this) and by routing (a different
model than the author's).

**The tester is not an LLM.** Verification runs the project's actual test
command (auto-detected: `go test`, `npm test`, `make test`, `pytest`;
overridable with `--test-cmd`) via a command executor. Cheap, fast, and the
resulting evidence is trustworthy.

**Executor abstraction.** `executor.Executor` is
`Run(ctx, Request) (Result, error)` where Request is prompt + workdir +
model hint + read-only flag, and Result is output text + usage/cost.
Implementations: `claude` (Claude Code CLI, `-p --output-format json`,
permissions mapped from the role: read-only roles get read/search tools only),
`mock` (scenario replay for tests/offline). Codex or a local model is one new
file. Nothing outside this package knows what Claude is.

**Router.** `router.Router` receives the signals an intelligent router needs
(role, uncertainty, attempt count, independence requirement, author model)
and returns executor + model + human-readable reason (recorded as a
`route.chosen` event — escalations are observable). The v0 implementation is
~40 lines of rules: cheap model first, strong model on retry/high
uncertainty/deep investigation, different-model reviews. A learned router
replaces one small interface.

**Isolation via git worktrees.** `gitws.Prepare` creates one worktree per
repo under `<data>/worktrees/<task-id>/<repo-name>` on branch
`orc/<task-id>`, recording base SHAs. Agents get that parent directory as
their working directory — a multi-repo task is just multiple subdirectories.
Diff = committed + uncommitted + untracked changes against the base SHA.
The user's checkouts are never touched; accepting the result is a normal
git merge of the task branch.

**Structured decisions.** Agents emit `decision_request` in their output;
the engine also generates decisions itself when loops exhaust their budgets.
A decision persists importance/question/recommendation/options and a
`return_to` phase; resolving it appends the human's choice to the task's
notes (visible to all subsequent agents) and re-enters the workflow. Options
carry an explicit `effect`: `abort` fails the task, `accept` completes it
as-is (confidence stays at whatever evidence was actually earned), `extend`
resets the step budget, and the default continues at `return_to`. Step
failures (agent timeouts, unparseable output) also pause on a decision
instead of discarding the task's accumulated work.

**Evidence model.** Append-only records with ordered levels:
`assumed < code_inspected < reproduced < implemented < tested < reviewed`.
Each is written by the step that earned it, pointing at its source (agent
run id or "tester"). The task's confidence is the strongest level reached —
reported in `GET /tasks/{id}`, never a boolean.

**Persistence & recovery.** All state transitions are persisted before and
after each step. On startup `RecoverInterrupted` flags tasks that died
mid-step; `resume` returns them to their last phase, and every step is a
valid resume point (workspace preparation is idempotent).

**Memory.** `memory.Store` interface with a JSONL implementation. Confirmed
`preference`/`project_rule` items scoped to a repo (or global) are injected
into every role prompt. `correction` items exist in the model as the input
for future pattern detection ("propose a rule after N similar corrections") —
that detector is deliberately not built yet.

## What was deliberately not built

No queues, no microservices, no generic agent framework, no LLM SDK
abstraction (the CLI *is* the abstraction boundary), no vector memory, no
auth, no frontend. See NEXT.md.
