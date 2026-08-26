# Proofline vertical slice — audit, design, acceptance

Scope of this iteration: prove one hypothesis — *after a coding agent changed a
real backend repo, can an engineer tell in 30–60 s what is proven, by what, and
what risk is still unknown?* Everything else in `plan.docx` (SaaS, RBAC,
GitHub App, sandboxing, billing, k8s) is out of scope and deliberately not
started.

## 1. Repository audit (2026-08-26)

`go build ./... && go vet ./... && go test ./...` — all green (engine tests run
the full workflow against real temp git repos with the mock executor).

| Area | State | Verdict for this slice |
| --- | --- | --- |
| `domain` — Task/TaskState, Event, Decision, Evidence (6 ordered levels), AgentRun, role outputs | solid, stdlib-only | **KEEP**, extend (claims/artifacts/packet/verdict) |
| `engine` — re-planning loop: understand → implement → verify → review; retries, budgets, decisions, recovery, resume | works, tested | **KEEP**; add baseline step + artifact capture + packet build hooks |
| `roles` — researcher / developer / reviewer prompts + JSON parsing; reviewer never sees developer reasoning (tested) | works | **KEEP**; ask researcher for a root-cause hypothesis, ask reviewer to *challenge* and report `checked` / `not_checked` |
| `verify` — real test execution (`go test`, `npm test`, …), not an LLM | works | **KEEP**; capture full output (currently 4 KB tail only) |
| `gitws` — one worktree per repo on `orc/<task>`; diff vs base SHA | works | **KEEP**; add `Commit` so the packet can point at a real SHA |
| `store` — file store: `task.json`, `events.jsonl`, `runs.jsonl`, `evidence.jsonl`, `decisions.json`; flock per task | works | **KEEP**; add `artifacts.jsonl`, `packets.jsonl`, `verdicts.jsonl` |
| `executor` — Claude CLI (`-p --output-format json`, read-only tool allow-list for researcher/reviewer) + scripted mock | works | **KEEP** as-is |
| `router` — rule-based, different model for reviewer | works | **KEEP** as-is |
| `api` — 8 JSON endpoints, no auth | works | **KEEP**; add packet/verdict endpoints + static UI |
| `memory` | works, unused by this slice | KEEP, untouched |
| `old/` ("hindsight" prototype) | dead | untouched |
| `reproduced` evidence level | declared, **never emitted** | this slice fixes that (baseline run) |
| Evidence records | `claim` is a free-form string, `detail` is the agent summary | not enough for a packet — new `Artifact` type carries the raw proof (command, exit code, output, diff, sha, findings) |

Gaps that matter for *this* slice only: no baseline reproduction, evidence has
no raw artifacts attached, no claim-level status, no separate human
merge decision (workflow `Decision` ≠ merge verdict), no UI.

Known gaps kept as-is (documented in NEXT.md): no auth, blanket Bash for the
developer inside the worktree, `test_command` over HTTP = command execution,
no CAS on `task.json`. They do not block the hypothesis.

## 2. Reuse map

| Proofline concept | Reused ORC entity |
| --- | --- |
| ChangeCase | `domain.Task` (+ `kind`, `repro_command`) |
| Baseline reproduction | new step inside `stepUnderstand`, uses `verify.Run` on the untouched worktree |
| Implementation | `stepImplement` + `gitws.Diff` (+ new `gitws.Commit`) |
| Verification | `stepVerify` (real tests) |
| Independent challenge | `stepReview` (reviewer: other model, no author reasoning, read-only) |
| Evidence artifacts | new `domain.Artifact`, written by the *same* step that produced the fact |
| Evidence levels / confidence | existing `domain.Evidence` (kept; packet references them) |
| Workflow pause / human input | existing `domain.Decision` |
| Merge decision | new `domain.Verdict` (accept / request_changes / reject) — separate from workflow decisions |
| Proof packet | new `domain.Packet`, built **only** from persisted artifacts/runs/evidence, versioned, append-only |

## 3. Minimal domain changes

```
Task        += Kind ("bugfix" | "change"), ReproCommand
TaskState   += Baseline []TestResult, BaselineDone bool, Commits map[repo]sha
Artifact     { ID, TaskID, Kind, Title, Repo, Command, ExitCode, Passed, Output,
               Files, Diff, Commits, Findings, Verdict, Checked, NotChecked,
               RunID, Phase, At }
             kinds: baseline_run | test_run | diff | review | root_cause
Claim        { Type, Title, Status, Statement, Reason, ArtifactIDs, EvidenceIDs, Gap }
             status: supported | insufficient | contradicted | blocked
Packet       { TaskID, Version, Verdict, Claims, Gaps, Risks, Change{...}, BuiltAt, Fingerprint }
Verdict      { ID, TaskID, PacketVersion, Decision, Note, By, At }
ResearchOutput += root_cause {statement, file, line}
ReviewOutput   += checked[], not_checked[], counterexample
```

Rules the packet builder enforces (pure function, unit-tested):

* `problem_reproduced` — SUPPORTED only if a `baseline_run` artifact exists and
  **failed**; CONTRADICTED if the baseline passed (the test does not exercise
  the bug); INSUFFICIENT if no baseline ran.
* `root_cause_supported` — SUPPORTED only if the researcher named a file, the
  file exists, the fix **modified that file**, and baseline failed → after-fix
  passed. A hypothesis alone is INSUFFICIENT.
* `change_verified` — SUPPORTED only if the repro command failed on baseline
  and passes after; plus full suite pass. Tests passing without a baseline
  failure = INSUFFICIENT (they may not exercise the bug). Failing = CONTRADICTED.
* `independent_challenge` — SUPPORTED if the reviewer (other model, no author
  reasoning) approved with no high-severity findings; CONTRADICTED if changes
  requested; INSUFFICIENT if no review ran. `not_checked` is surfaced as gaps.
* `integration_checked`, `cross_service_impact` — no runner exists in this
  slice → always INSUFFICIENT, shown as first-class gaps. This is the
  deliberate honest hole.
* Verdict: BLOCKED if task failed or any core claim CONTRADICTED; INSUFFICIENT
  if any core claim not supported; else SUPPORTED (gaps still listed).
* Agent free text (summaries, risks) is labelled *agent-reported*, never
  counted as evidence.

## 4. UI information architecture

Central screen = `/cases/<task-id>` (Change Case). Index `/` is only a list.

```
L1  VERDICT band  ·  CHANGE (goal, kind, repo, branch, sha, files)  ·  CLAIMS  ·  GAPS  ·  RISKS  ·  HUMAN DECISION
L2  click a claim → its artifacts: command, exit code, baseline vs after output, diff, file list, reviewer findings / checked / not-checked
L3  "Raw" tab → agent runs (model, cost, duration), full event log, evidence records, packet versions
```

## 5. Wireframe

```
┌──────────────────────────────────────────────────────────────────────────┐
│ CHANGE CASE  task_1a2b   Fix duplicate reservation across timezones      │
│ ████ INSUFFICIENT ████   2 core claims supported · 1 gap · 2 unverified   │
├──────────────────────────────────────────────────────────────────────────┤
│ CHANGE   repo reservations · branch orc/task_1a2b · commit 9f3c1e2       │
│          files: reservations/store.go, reservations/store_test.go        │
├──────────────────────────────────────────────────────────────────────────┤
│ CLAIMS                                                                    │
│  ✔ SUPPORTED    Problem reproduced        go test -run Dup → FAIL (exit 1)│
│  ✔ SUPPORTED    Root cause supported      store.go:31 · modified by fix  │
│  ✔ SUPPORTED    Change verified           baseline FAIL → after PASS     │
│  ✔ SUPPORTED    Independent challenge     opus · approve · 1 low finding │
│  ▲ INSUFFICIENT Integration checked       no integration check configured│
│  ▲ INSUFFICIENT Cross-service impact      HTTP handler not exercised     │
├──────────────────────────────────────────────────────────────────────────┤
│ NOT VERIFIED (loud)   · handler path · concurrency under parallel calls   │
│ UNRESOLVED RISKS      · reviewer: low — …  · agent-reported: …            │
├──────────────────────────────────────────────────────────────────────────┤
│ HUMAN DECISION   [ Accept ]  [ Request changes ]  [ Reject ]   note: ____ │
│ (recorded separately from the agent verdict; survives refresh)           │
├──────────────────────────────────────────────────────────────────────────┤
│ ▸ Evidence (per claim, expand)   ▸ Raw: runs · events · packet versions   │
└──────────────────────────────────────────────────────────────────────────┘
```

## 6. Fixture bug and expected evidence

`fixtures/reservations` — tiny Go module, in-memory reservation store used by
an HTTP handler.

Bug: `Store.Reserve(room, day time.Time)` de-duplicates by key
`room + "/" + day.Format("2006-01-02")` but does **not** normalise the
instant to UTC first, so the same calendar day expressed in two timezones
produces two keys and a double booking is accepted. `TestReserveRejectsSameDayAcrossTimezones`
fails on baseline; `TestReserveRejectsExactDuplicate` and `TestReserveDifferentDays`
pass. `handler.go` (HTTP layer) has no tests — the deliberately unchecked aspect.

Expected persisted evidence for a successful run:

| Claim | Artifact |
| --- | --- |
| Problem reproduced | `baseline_run`: `go test ./...` exit 1, output contains `--- FAIL: TestReserveRejectsSameDayAcrossTimezones` |
| Root cause | `root_cause` from researcher run: `reservations/store.go` line of `dayKey`; `diff` artifact shows `store.go` modified |
| Change verified | `test_run`: same command exit 0 after the change, `PASS` |
| Independent challenge | `review` artifact: verdict, findings, checked / not_checked, from a different model |
| Integration / cross-service | **no artifact** → INSUFFICIENT |
| Human decision | `verdicts.jsonl` record |
