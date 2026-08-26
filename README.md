# orc — engineering task orchestrator (prototype)

A working vertical slice of an orchestrator that runs engineering tasks
through coding agents so the user doesn't have to babysit them. You give it
git repositories and a task; it researches, implements, verifies with real
tests, gets an independent review, and records everything as structured
events, agent runs and evidence — persisted on disk, resumable after restart.

Written in Go, stdlib only. One binary, no external services.

## Prerequisites

- Go **1.23+** (`go.mod` pins `go 1.23`); git; `make`.
- macOS for `SAFE_SANDBOX` (uses `/usr/bin/sandbox-exec`). Linux builds and
  runs in `LOCAL_UNSAFE` only until a container boundary exists.
- Optional: `claude` CLI (real agents), Google Chrome (browser e2e),
  Docker (`make docker` builds the Linux image).

`make verify` = build + vet + all tests + race subset + product acceptance.

## Open the app (Local Pilot)

```bash
go build -o bin/orc ./cmd/orc
./bin/orc serve --addr 127.0.0.1:8080 --data ./.orchestrator
open http://127.0.0.1:8080
```

The app is a single embedded page over the real API: **Cases** (overview,
decisions waiting), **New change case** (local repository or GitHub PR —
shows "not connected" until credentials exist), **Case** (Proof packet ·
Timeline · Packet history · Raw), **Repositories & settings** (execution
mode and what is actually enforced), **Help** (what SUPPORTED / INSUFFICIENT
/ STALE / CONTRADICTED / BLOCKED mean).

"Load example" runs the embedded reservations fixture (a real timezone
double-booking bug) through the **real** engine — worktree, baseline, `go
test -v`, original-test replay, packet — with scripted agent replies. Such
cases are labelled *Local Pilot example* everywhere. Real agents need the
`claude` CLI; set `PROOFLINE_SANDBOX=SAFE_SANDBOX` (macOS) to confine them.

## Quick start

```bash
go build -o bin/orc ./cmd/orc
```

### Run a real task with Claude Code

Requires the `claude` CLI installed and authenticated.

```bash
./bin/orc create \
  --repo ~/work/project-a \
  --repo ~/work/shared-lib \
  --task "Fix incorrect retry behaviour" \
  --repro-cmd "go test -run TestRetry ./..." \
  --data ./.orchestrator
```

The CLI streams structured events live and prints the final state:
which files changed, in which worktrees (your repos are never touched —
work happens on branch `orc/<task-id>` in isolated git worktrees), the
confidence level reached, and cost/token totals.

### Run offline (no LLM) with the mock executor

The mock executor replays a scenario file — the entire workflow (worktrees,
diffs, real `go test` runs, evidence, events) executes for real:

```bash
./bin/orc create --repo ./some-repo --task "..." \
  --executor mock --script scenario.json
```

See `internal/executor/script.go` for the scenario format.

### Proofline: the Change Case (claims → evidence → decision)

Every task produces a **proof packet** built strictly from persisted
artifacts (baseline runs, test runs, diff + commit, reviewer output). Free-form
agent text is never evidence; a missing artifact means `INSUFFICIENT`, a
contradicting one means `CONTRADICTED`/`BLOCKED`.

```bash
# bugfix-shaped task: the repro command must FAIL on the baseline and PASS after
./bin/orc create --repo ./reservations \
  --task "Fix duplicate reservation across timezones" \
  --repro-cmd "go test -run TestReserveRejectsSameUTCDayAcrossTimezones ./..."
./bin/orc packet <task-id>                       # verdict, claims, gaps, risks
./bin/orc decide <task-id> --decision request_changes --note "..."
./bin/orc serve --addr 127.0.0.1:8080            # then open /cases/<task-id>
```

Verify an **existing** change (a PR head, branch or SHA) instead of letting an
agent write one — the base is the repo's HEAD, the head is verified and
challenged, no developer runs:

```bash
./bin/orc verify --repo ./reservations --head fix-branch --pr acme/reservations#7 \
  --task "Fix duplicate reservation across timezones" \
  --repro-cmd "go test -run TestReserveRejectsSameUTCDayAcrossTimezones ./..."
./bin/orc github-status <task-id>          # prints the commit status + PR comment; --post needs GITHUB_TOKEN
```

Offline scenario suite (real worktrees + real `go test`, scripted agents):
`scripts/run-scenarios.sh /tmp/scen-data` then `orc serve --data /tmp/scen-data`.
The matrix of expected vs actual verdicts is in `docs/SCENARIOS.md`;
adversarial findings in `FALSE_PROOF_REPORT.md`.

Try it on the shipped fixture (real timezone dedup bug, one failing test):
`scripts/fixture-repo.sh /tmp/reservations`. See `docs/PROOFLINE_SLICE.md`.

### HTTP API

```bash
./bin/orc serve --addr :8080 --data ./.orchestrator
```

| Endpoint | Description |
| --- | --- |
| `POST /tasks` | `{"repos": [...], "goal": "...", "context": [...]}` — creates and starts a task |
| `GET /tasks` | list tasks |
| `GET /tasks/{id}` | full structured state: task, runs, evidence, decisions, confidence, totals |
| `GET /tasks/{id}/events?after=N` | event log (poll with `after` for incremental updates) |
| `GET /tasks/{id}/decisions` | decisions |
| `POST /tasks/{id}/decisions/{did}/resolve` | `{"option": "id", "note": "..."}` — resolve and continue |
| `POST /tasks/{id}/resume` | resume an interrupted task |
| `GET /tasks/{id}/packet` | change case: latest packet + artifacts, verdicts, runs, versions |
| `GET /tasks/{id}/packet/versions/{v}` | a historical packet version (immutable) |
| `POST /tasks/{id}/verdict` | `{"decision":"accept|request_changes|reject","note":"..."}` — human merge decision |
| `GET /` , `GET /cases/{id}` | embedded Change Case UI (reads the API above) |

### Other commands

```bash
./bin/orc list                       # all tasks
./bin/orc show <task-id>             # full state as JSON
./bin/orc events <task-id>           # event log
./bin/orc resolve <task-id> <dec-id> --option retry --note "try X"
./bin/orc resume <task-id>           # after a crash/interrupt
./bin/orc run <task-id>              # start a task created with --no-run
./bin/orc memory add --kind project_rule "Do not add comments that merely restate code."
./bin/orc memory list
```

Memory items (`preference`, `project_rule`) are injected into every agent
prompt for matching repos.

## How a task flows

```
UNDERSTAND ──► IMPLEMENT ──► VERIFY ──► REVIEW ──► DONE
     │              │           │           │
     │ high         │ blocked/  │ tests     │ changes
     │ uncertainty  │ uncertain │ fail      │ requested
     ▼              ▼           ▼           ▼
INVESTIGATE ◄───────┘      IMPLEMENT   IMPLEMENT
     │                     (retry,     (with findings)
     ▼                      escalated
 IMPLEMENT                  model)
```

Any step can pause the task on a **structured decision** (question,
recommendation, options) that a human resolves via CLI or API; repeated
failures escalate to a decision automatically.

The task's outcome carries an **evidence chain**, not a boolean:
`assumed → code_inspected → reproduced → implemented → tested → reviewed`.
"The agent thinks it's fixed" and "tests pass and an independent reviewer
confirmed" are different confidence levels, and the API reports which one
you actually have.

## Testing

```bash
go test ./...
```

Engine tests run the full workflow against real temp git repositories with
the mock executor — including failure loops, investigation branches,
decisions, multi-repo tasks and crash recovery.

See `ARCHITECTURE.md` for design, `NEXT.md` for honest status and next steps.
