# NEXT — honest status and next steps

## What really works (verified by running it)

- **Full end-to-end with real Claude Code**: `orc create --repo <buggy retry
  lib> --task "Fix incorrect retry behaviour..."` ran researcher (sonnet) →
  developer (sonnet, produced the correct one-line fix in an isolated
  worktree) → real `go test` (passed) → independent reviewer (opus, approved
  with 2 recorded low-severity findings) → done. 3 agent runs, ~90s, $0.47,
  confidence `reviewed`. The original repo was untouched; the fix sits on
  branch `orc/<task-id>`.
- **Offline end-to-end with the mock executor** — same workflow, real
  worktrees/diffs/test runs, zero LLM calls.
- **Failure loops**: tests fail → re-implement with failure output → model
  escalation (router) → after budget, a structured decision. Observed live
  when a broken scenario produced no changes: the system retried twice and
  paused on a decision instead of looping or lying.
- **Structured decisions** over CLI and HTTP: create → task pauses →
  resolve with option + note → note becomes agent-visible guidance → workflow
  continues. `abort` fails the task.
- **Persistence + crash recovery**: kill mid-task → restart →
  `RecoverInterrupted` marks it interrupted → `resume` continues from the
  last phase (unit-tested).
- **Multi-repo tasks**: worktree-per-repo under one working directory,
  combined diff (unit-tested).
- **HTTP API**: create/list/get/events-after-seq/decisions/resolve/resume,
  all returning structured state (smoke-tested end to end).
- **Observability**: every agent run persists role, executor, model, route
  reason, duration, tokens, cost; escalations appear as `route.chosen`
  events with reasons.
- **Evidence chain**: `code_inspected → implemented → tested → reviewed`
  populated by the real run; `GET /tasks/{id}` reports the strongest level
  as `confidence`.

## Proofline slice (2026-08-26) — verified by running it

- Real run on `fixtures/reservations` (timezone dedup bug): baseline
  `go test -run …Timezones` FAIL (exit 1) → sonnet fix in `store.go`, committed
  `e9a8524` → repro + full suite PASS → opus review approve, 3 low findings,
  12 checked / 7 explicitly not-checked → packet v1 `SUPPORTED` with 9 gaps
  → human `request_changes` recorded, survives server restart. 3 runs, $0.58.
- Claims are derived only from `artifacts.jsonl` (`internal/proof`, pure,
  unit-tested). Baseline-passes → `problem_reproduced: CONTRADICTED` and
  verdict `BLOCKED` (tested). Integration / cross-service are always
  `INSUFFICIENT` — there is no runner; the UI shows them as gaps, on purpose.
- Packets are append-only versions with a content fingerprint; verdicts pin a
  packet version and are refused while the workflow is running.

Sprint 2 (same day): adversarial pass — see `FALSE_PROOF_REPORT.md`. Every
artifact is now bound to the worktree HEAD per repo (`source_shas`); claims
accept only artifacts on the current state, otherwise STALE. `go test`/`pytest`
run with `-v`, per-test results are persisted and the baseline-failing tests
must be observed passing. Author-modified test files trigger a replay of the
original tests against the changed code. Narrow repro runs twice (flaky
guard). Verify-only mode (`orc verify --head`) for existing changes/PR heads;
`internal/github` builds statuses/comments that are never a fake green.
Scenario matrix in `docs/SCENARIOS.md`; honest reviews in
`PRODUCT_HYPOTHESIS_REVIEW.md` and `DOGFOOD_READINESS.md`.

Known thin spots of the slice:
- `root_cause_supported` is a cross-check (file named ∧ file modified ∧
  fail→pass), not a proof of causality.
- If the developer edits test files, the after-change run uses different
  tests than the baseline; the packet flags it as a risk but still counts the
  same-command flip.
- No integration/log/trace runner; no cross-repo test.
- The reviewer can now run `go test`/`npm test`/`pytest` (allow-listed) but
  nothing forces it to; `not_checked` is self-reported.
- UI is one embedded HTML file polling the API; no SSE, no auth.

## Foundation sprint (2026-08-26, later) — see FABLE_HANDOFF.md

- **Execution boundary** `internal/sandbox`: argv-only commands with a
  runner allowlist, constructed env, process-group kill, caps, redaction,
  worktree scan (symlink escape / submodules / nested repos → BLOCKED),
  hardened git; `SAFE_SANDBOX` (macOS sandbox-exec, verified: ~/.ssh & net
  denied for commands, Read-tool EPERM for the Claude CLI) vs `LOCAL_UNSAFE`
  (default, loud). Repo registry with IDs; SAFE refuses raw paths.
- **State**: CAS `task.json` (Version), per-task lease, idempotent creation,
  verdict pinned to the viewed packet version (409 on change), packet
  version serialisation, effects ledger (at-most-once GitHub posts).
- **Evidence**: explicit claim policies + scope in the packet, source state
  captured before/after commands, completeness flags (truncated/redacted/
  timed out). `EVIDENCE_INVARIANTS.md` I1–I13 mapped to tests.
- **GitHub**: webhook HMAC + delivery idempotency, PR import as verify-only
  case bound to base/head SHA, revocation → BLOCKED, fake GitHub server;
  real GitHub NOT VERIFIED (no token).
- **Auth**: workspaces/memberships/tokens, permission matrix, central
  middleware, cross-tenant tests; local single-user mode only on loopback.
- **Lifecycle**: crash/cancel/restart tests; unknown work is never done.

## What is fake / stub / thin

- **The router is 40 lines of if-else.** Deliberate: the *interface* carries
  the right signals (role, uncertainty, attempts, independence, author
  model), but nothing is learned or cost-aware yet. Costs are recorded but
  not used for routing.
- **Memory pattern detection does not exist.** Storage + prompt injection of
  confirmed rules works; `correction` items and the `proposed` status are
  dead weight until a detector proposes rules from repeated corrections.
- ~~`reproduced` evidence level is never emitted.~~ Done: baseline runs
  before research for every task; bugfix tasks emit `reproduced` on failure.
- **The Tester role has no LLM mode.** Fine for repos with test suites;
  tasks without one silently skip to review (an event records the skip).
- **`uncertainty` is self-reported** by the researcher. There is no
  calibration; a confidently wrong researcher routes the task down the
  cheap path.
- **Claude executor uses coarse permissions** (`acceptEdits` + broad Bash
  allow inside the worktree). Worktree isolation limits file damage, but a
  malicious/confused agent could still run arbitrary commands. No sandboxing.
- **One task = one engine goroutine**; no queue, no parallel steps, no
  per-task cancellation via API (only process signals).
- **Event bus is a callback + file polling.** Fine for CLI/polling UI; no
  SSE/websocket push yet.

## Independent review: fixed vs still open

A subagent reviewed the codebase adversarially. Fixed as a result:

- **"Accept as-is" decision option did nothing** (sent the task back to
  implementation forever). Decision options now carry an explicit `effect`
  (`abort` / `accept` / `extend` / continue); accept completes the task and
  confidence honestly stays at `tested`, not `reviewed`.
- **`pending` tasks were unstartable** (`--no-run` / `start:false` was a dead
  end). Added `orc run <id>` and `POST /tasks/{id}/run`.
- **`ResolveDecision` had no status guard** — a stale decision could
  resurrect a done/failed task or yank a running one mid-step. Now rejected
  unless the task is `awaiting_decision`.
- **Any agent error permanently failed the task**, discarding all prior work
  (e.g. a reviewer timeout after implement+verify). Step failures now pause
  on a decision (retry with guidance / abort); step-budget exhaustion offers
  `extend` instead of hard-failing.
- **Crash between `worktree add` and task save lost base SHAs**, hard-failing
  every later diff. Base SHA is now written to a sidecar file before worktree
  creation and recovered on resume.
- **CLI + server on one data dir could drive the same task twice.** Added a
  per-task cross-process flock.
- **Torn last JSONL line after kill -9 poisoned the event log forever**
  (appends silently failed while the task kept running). Reads now tolerate
  a torn final line.
- Smaller: ctx cancellation during verify no longer burns a fix attempt;
  npm's placeholder `no test specified` script is no longer treated as a
  test suite; reviewer diff prompt is capped at 150KB; mock scenario paths
  can't escape the worktree; duplicate `--repo` paths rejected; review
  findings are no longer dropped when the developer reports blocked;
  `Diff` no longer leaves intent-to-add entries in the index.

Still open (known, deliberate for now):

- **No compare-and-swap on `task.json`** — engine and resolve endpoints do
  read-modify-write; the awaiting-decision guard closes the practical
  window, but a version field + CAS is the proper fix.
- **Event `Seq` can duplicate across processes** (per-process lazy counter).
  The flock prevents two writers per task, so this is theoretical until
  something else appends events.
- **Attempt numbering is overloaded** (scenario key, telemetry, and the
  parse-retry `attempt+1` collide) — see refactor list below.
- **`test_command` and repo paths via HTTP = arbitrary command execution**
  for anyone who can reach the port. Bind to localhost and/or add a token
  before exposing `serve` beyond the machine.
- **Developer role has blanket Bash** inside the worktree (acceptEdits +
  `Bash`) — worktree isolation is git-level, not OS-level. Container
  sandboxing is a later step. Same class: a scripted `package.json` test
  entry written by the developer runs via `sh -c` in verify.
- **Events endpoint re-reads the whole JSONL per poll** — fine at prototype
  scale, SQLite fixes it properly.

## Where the architecture turned out weak

- **Status doubles as "phase" and "what to do next".** It works, but the
  planner logic lives half in `RunTask`'s dispatch and half in each step's
  tail. A real intelligent planner ("given everything that happened, what
  now?") wants one explicit decision point; extracting a `Planner` interface
  is the first refactor worth doing.
- **Scripted mock keys on repo-relative paths** (`repoA/file.go`), so
  scenarios silently break when the repo directory is named differently —
  cost me a confusing debugging session. Scenario files should validate
  their paths against the workspace.
- **`TaskState` is a growing grab-bag.** Each new branch of the workflow
  added fields. At some point role-specific state should move into typed
  step records derived from events, not one struct.
- **Attempt numbering is spread around** (`ImplementAttempts`,
  `FixAttempts`, `Investigations`, `ReviewRounds`, plus the parse-retry
  offset). It's correct today but easy to break; needs consolidation into
  an explicit per-step attempt model.

## Assumptions to validate next

1. Do agent-emitted JSON contracts hold up on messy real tasks, or do we
   need schema-forced output (Claude CLI supports structured output modes)?
2. Is diff-only context enough for the reviewer on multi-file changes, or
   does it need a file-tree + targeted file reads budget?
3. Does uncertainty-driven investigation actually reduce failure rate vs
   always-implement? (Measure: fix attempts per task with/without.)
4. Are worktrees enough isolation for `Bash`-capable agents, or do we need
   containers per task?
5. Will the file store hold up at ~100 tasks with a polling UI, or is SQLite
   needed sooner than expected?

## 10 next engineering steps

1. Extract an explicit `Planner` interface (state in → next step + reason
   out); move the branch logic out of step tails. This is the seam for an
   LLM-driven planner later.
2. Baseline verification before implementation for bugfix tasks → emit
   `reproduced` evidence; feed the failing output to the developer.
3. Enforce structured agent output via the CLI's native mechanisms instead
   of prompt-and-parse (retry-on-parse-failure already exists as fallback).
4. SSE endpoint (`GET /tasks/{id}/events?follow=1`) on top of the existing
   `OnEvent` hook, so a UI doesn't poll.
5. Task cancellation API (`POST /tasks/{id}/cancel`) — engine already
   handles context cancellation; it just needs a per-task cancel func
   registry.
6. Cost-aware routing v1: budgets per task (`max_cost_usd`), router refuses
   escalation past budget and raises a decision instead.
7. Second real executor (Codex CLI or a local model via an OpenAI-compatible
   endpoint) to prove the executor abstraction with a non-Claude backend —
   and enable cross-vendor independent review.
8. Memory pattern detector v0: count similar `correction` items (embedding
   or even normalized-text match), propose a `project_rule` as a decision
   for the user to confirm.
9. Worktree lifecycle: `orc merge <task-id>` / `orc discard <task-id>` to
   land or clean up task branches; prune worktrees on task failure.
10. A tiny read-only debug UI (single static HTML polling the API) — only
    after SSE, and only one page.

## What NOT to build yet

- No queues/brokers, no microservices — one process is nowhere near its
  limits.
- No vector memory / RAG — there is no retrieval problem yet; rules fit in
  the prompt.
- No generic agent framework or plugin system — three roles and two
  executors do not justify abstraction layers.
- No auth/multi-tenancy — local single-user tool.
- No parallel agent fan-out inside a task — sequential with branching is
  still the bottleneck-free path; parallelism belongs after the Planner
  extraction.
- No polished frontend — the API is the product surface for now.
