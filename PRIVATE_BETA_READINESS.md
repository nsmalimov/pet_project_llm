# PRIVATE_BETA_READINESS — 2026-08-26 (final for this iteration)

Can one throwaway repository go into a private beta on this build?
**Yes — on one trusted macOS host in SAFE_SANDBOX, Local Pilot or a
manually imported PR, with the live-GitHub steps run per runbook first.
No — for untrusted repositories, Linux hosting, or anything beyond one
machine.** Not production-ready (P0 blockers at the end).

| Capability | Status | Evidence | Remaining risk |
|---|---|---|---|
| Build | **VERIFIED** (local) / **NOT VERIFIED** (CI, Docker) | `make verify` → `verify: OK`; `go.mod` pins 1.23; BUILD_READINESS.md; `.github/workflows/verify.yml` and `Dockerfile` never executed here (no Docker daemon, CI not run) | clean-Linux build unproven |
| Product UI | **VERIFIED** | `TestBrowserDecisionScreen` (headless Chrome DOM: empty state, form honesty, packet, timeline, decision + reload, history, 404); screenshots reviewed | polling (2 s), no SSE; no diff viewer |
| Local Pilot | **VERIFIED** | `TestAcceptanceScenariosOverHTTP`: A supported (+integration supported), B/C/E blocked, stale after edit → re-verify → supported v≥3, cancel → interrupted → resume → done; `TestExampleCaseEndToEndOverHTTP`; manual server restart | scripted agents in examples (labelled) |
| Real agents via web path | **VERIFIED once** | task_24958557: registered repo → `POST /tasks` → Claude sonnet/opus → SUPPORTED, 9 gaps, $1.04 | cost; reviewer coverage self-reported |
| GitHub fake path | **VERIFIED** | `TestGitHubPRLifecycle` (A verified → B pushed → one case per head, A immutable, NOT VERIFIED for B, duplicate deliveries, revocation → BLOCKED, effects ledger refuses replay), `TestWebhookSignatureAndParsing`, `TestFetchPRAndRevocation` | — |
| GitHub live path | **NOT VERIFIED** | no token; runbook `REAL_GITHUB_PILOT.md`; UI shows "not connected"; posting button disabled | one shared webhook secret; PR listing not implemented |
| Repository policy | **VERIFIED** | `TestIntegrationCheckEndToEnd` (commands from policy), `TestRepositoryPolicyEnforcedOverHTTP` (allowed_runners, agent_write=false → verify-only, hostile policy rejected), `TestSafeModeRefusesRawPathsAndOutsideRoots` | retention_days informational only (`orc gc` is manual) |
| Integration check | **VERIFIED** (HTTP provider) | `TestIntegrationCheckEndToEnd`: service started from worktree, 3 checks; baseline fail → after pass → SUPPORTED bound to SHA; unfixed head → CONTRADICTED; unavailable/timeout → INSUFFICIENT (code path; unit-level via outcome field) | one provider (HTTP); no compose/datastore; cross-service INSUFFICIENT by design |
| Auth/workspace | **VERIFIED** (API) / **PARTIALLY VERIFIED** (UX) | `authz_test.go` (401/403/404, cross-tenant, revoked, header escalation), `TestTokenIssueRevokeAndWorkspaces` (issue/list/revoke, members, owner-only); UI sign-in, workspace switcher, tokens, members | first-user setup is CLI (`orc auth init`); no SSO; tokens in browser localStorage |
| Sandbox | **VERIFIED** (macOS SAFE_SANDBOX) / **PARTIALLY VERIFIED** (agent) / **BLOCKED** (Linux) | sandbox adversarial tests, engine security tests, gitws hooks test, live Claude probe (once); default LOCAL_UNSAFE labelled everywhere | default is unsafe; agent keychain access; Linux none |
| Recovery/cancel | **VERIFIED** | lifecycle, concurrency, `TestCancelOverHTTPLeavesInterrupted`, acceptance interrupted→resumed; graceful shutdown implemented (`Engine.Shutdown`) — **NOT VERIFIED** by test | shutdown untested |
| Evidence integrity | **VERIFIED** | EVIDENCE_INVARIANTS.md I1–I13 ↔ tests; FALSE_PROOF_REPORT.md; scenario matrix | semantic fake fix; self-reported reviewer coverage; Go/pytest test identity only |
| Operations/deployment | **PARTIALLY VERIFIED** | `/health`, `/ready` (writability probe), `/metrics` (derived from persisted state), request logs with `X-Request-ID`, failure taxonomy (`failure_kind`), `orc backup/restore` (`TestBackupRestoreRoundTrip`), `orc gc` | never deployed; no TLS; file store; no alerts |

## Files changed (this iteration)
`.github/workflows/verify.yml`, `Makefile`, `Dockerfile`, `go.mod`, `README.md`,
`internal/repos` (policy fields/enforcement), `internal/integration` (outcomes),
`internal/proof` (integration INSUFFICIENT on unavailable/timeout),
`internal/engine` (policy enforcement, Shutdown, failure taxonomy),
`internal/auth` (tokens/members listing), `internal/api` (tokens, workspaces,
members, ready, metrics, Logged), `internal/api/ui.html` (workspace/tokens),
`cmd/orc` (graceful shutdown, backup, restore), tests: `acceptance_test.go`,
`tokens_test.go`; docs: BUILD_READINESS.md, PILOT_RUNBOOK.md, this file.

## Commands run
`gofmt -l .`, `go vet ./...`, `go build -o bin/orc ./cmd/orc`, `go test ./...`,
`go test -race` (subset via `make verify`; full race run earlier today green),
`make verify` → `verify: OK`, headless Chrome screenshots, manual restart.

## Real vs scripted
Real: worktrees, baseline/test/integration/replay commands, commits, SHAs,
artifacts, packets, verdicts, audit, cancel/resume/reverify, Claude agents
when used. Scripted: agent replies in "Local Pilot example" cases only
(labelled). Fake: nothing; `fakegh` exists only in tests.

## Target OS / execution mode
macOS, `SAFE_SANDBOX` for the pilot; `LOCAL_UNSAFE` default for development.
Linux: build only.

## External services / credentials used
Anthropic via the local `claude` CLI (one real run). No GitHub credentials.

## Remaining unsafe paths
1. LOCAL_UNSAFE default (loud). 2. Linux has no boundary. 3. Agent under
SAFE_SANDBOX can read the login keychain and has network. 4. Regex redaction.
5. Single webhook secret; webhooks refused in LOCAL_UNSAFE by default.
6. Integration service port collisions between concurrent cases with the same policy port.

## NOT VERIFIED
CI workflow run, Docker build, GitHub live path, graceful shutdown under load, non-loopback deployment.

## P0 blockers before production
1. Linux/container execution boundary. 2. Live GitHub verification.
3. Verified clean-environment build (CI/Docker). 4. TLS/reverse proxy and
token delivery for non-loopback. 5. Graceful shutdown test.

## Recommended next step
Run `REAL_GITHUB_PILOT.md` with a throwaway repo and a scoped token; then
push the CI workflow and fix whatever the clean Linux build reveals. Only
after both: decide on the Linux boundary (bwrap vs container).
