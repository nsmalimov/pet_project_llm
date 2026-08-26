# PRIVATE_BETA_READINESS — 2026-08-26

Question this answers: **can one throwaway repository go into a private
beta on this build?** Short answer: **yes, on a trusted macOS machine in
SAFE_SANDBOX, with Local Pilot or a manually imported PR; no for untrusted
repositories or Linux hosting.** The project is not production-ready (see
P0 blockers).

| Capability | Status | Evidence |
|---|---|---|
| Build | **VERIFIED (macOS/Go 1.27 toolchain, go.mod pins 1.23)** / **BLOCKED (clean Linux image)** | `go build ./...`, `make verify` → `verify: OK` (build, vet, `go test ./...`, race subset, product acceptance). `Dockerfile` written; `docker build` NOT run — Docker daemon unavailable on this machine. |
| Local Pilot product path | **VERIFIED** | `TestBrowserDecisionScreen` (headless Chrome DOM): empty overview → new-case form with honest not-configured states → real example run → BLOCKED packet with contradiction → human decision → reload shows pinned decision → history → 404 honesty. `TestExampleCaseEndToEndOverHTTP`, `TestCancelOverHTTPLeavesInterrupted`. Manual: three examples via UI, decision, **server restart**, state intact. |
| Real agents through the web path | **VERIFIED once** | Registered repo + `POST /tasks` with Claude executor: SUPPORTED, 4/4 core claims, 9 gaps, $1.04 (task_24958557 in `.orchestrator`). |
| Real GitHub path | **NOT VERIFIED (live)** / VERIFIED against fake server | `TestGitHubPRLifecycle`, `TestWebhookSignatureAndParsing`, `TestFetchPRAndRevocation` against `internal/github/fakegh`. No token here. Runbook: `REAL_GITHUB_PILOT.md`. |
| Sandbox | **VERIFIED (macOS SAFE_SANDBOX)** / **PARTIALLY VERIFIED (agent)** / **BLOCKED (Linux/container)** | `TestSafeSandboxDeniesNetworkAndHostSecrets`, `TestSafeSandboxCanRunGoTests`, `TestCommandInjectionRejected`, `TestPathTraversalAndSymlinkEscape`, `TestBackgroundChildrenAreKilledOnTimeout`, `TestHostEnvironmentNotInherited`, `TestOutputCapAndRedaction`, `TestMaliciousHooksAndFsmonitorNeverRun`, engine `security_test.go`. Live Claude probe under the agent profile passed once (`PROOFLINE_LIVE=1`). Default mode is LOCAL_UNSAFE and is labelled on every screen, `/system`, packets and artifacts. |
| Repository policy | **VERIFIED** | `TestIntegrationCheckEndToEnd` (commands come from policy; SAFE mode refuses ad-hoc commands — `TestSafeModeRefusesRawPathsAndOutsideRoots`). |
| Integration check | **VERIFIED (HTTP provider)** | `TestIntegrationCheckEndToEnd`: service started from the worktree, 3 checks, fails on baseline / passes after fix → SUPPORTED bound to SHA; unfixed head → CONTRADICTED → BLOCKED. Example A in the UI shows it. Cross-service stays INSUFFICIENT by design. |
| Auth/workspace | **PARTIALLY VERIFIED** | `internal/api/authz_test.go` (401/403/404 matrix, cross-tenant, revoked membership, header escalation); token entry + sign-out in the UI; `GET /whoami`; audit log (`GET /audit`, UI page). Not verified: token rotation UX, non-loopback deployment. |
| Recovery/cancel | **VERIFIED** | `lifecycle_test.go` (crash during research/implementation/tests, packet rebuild, stale lease), `concurrency_test.go` (CAS, idempotency, two workers, verdict pinning, cancel kills children, packet version serialisation), `TestReverifyAfterEditProducesFreshEvidence`. |
| Production deploy | **NOT VERIFIED** | Never deployed. Blockers: Docker build unverified; no Linux execution boundary; no TLS/reverse-proxy story; file store only. |

## Files changed (this step)
`go.mod` (go 1.23), `Dockerfile`, `Makefile` (verify), `README.md`,
`fixtures/embed.go` + `fixtures/reservations/cmd/server/main.go.fixture`,
`internal/repos` (Policy, IntegrationCheck, SetPolicy), `internal/sandbox/proc.go`
(+ LocalNetwork profile), `internal/integration/`, `internal/engine`
(policy application, integration runs, reverify, audit), `internal/proof`
(integration claim), `internal/domain` (AuditRecord, integration artifact),
`internal/store` (audit), `internal/api` (policy/audit/whoami endpoints,
GitHub post button, UI), `cmd/orc` (repo policy, gc), tests, `REAL_GITHUB_PILOT.md`.

## Commands run
`go vet ./...`, `go test ./...` (all ok), `go test -race` subset (ok),
`make verify` (OK), headless-Chrome screenshots of overview / packet /
timeline / new-case, manual server restart check.

## Real vs scripted
- Real everywhere: worktrees, baseline/test/integration commands, commits,
  SHAs, artifacts, packets, verdicts, audit, cancel/resume, Claude agents
  when the `claude` CLI is used.
- Scripted: only the agents' replies in "Local Pilot example" cases
  (executor `scenario`), labelled on the case, overview and runs table.
- Fake: nothing. The fake GitHub server exists only in tests.

## Remaining unsafe paths
1. LOCAL_UNSAFE default: commands and agents reach the host. Loud, but a
   default.
2. SAFE_SANDBOX is macOS-only; the agent profile allows the login keychain
   (CLI auth) and network.
3. Developer agent may run `go test`/`go build` etc. on repository code —
   arbitrary code by construction; only the OS sandbox bounds it.
4. Redaction is pattern-based.
5. One webhook secret for all workspaces; webhooks refused in LOCAL_UNSAFE
   unless explicitly allowed.
6. Integration services listen on localhost with a policy-chosen port; two
   concurrent cases with the same port collide (example policy randomises).

## First-pilot runbook (trusted macOS host, one throwaway repo)
```bash
make build
export PROOFLINE_SANDBOX=SAFE_SANDBOX
export PROOFLINE_REPOS_ROOT=$HOME/pilot-repos
./bin/orc auth init --workspace pilot --user you        # prints the token once
./bin/orc repo add $HOME/pilot-repos/REPO [--github owner/REPO]
./bin/orc repo policy <repo_id> --file policy.json      # approved test/repro (+ optional HTTP check)
./bin/orc serve --addr 127.0.0.1:8080 --data ./.orchestrator
# open http://127.0.0.1:8080 → paste token → New change case → Start verification
```
GitHub posting: follow `REAL_GITHUB_PILOT.md` and mark the row VERIFIED only
after the six steps pass.

## P0 blockers before anything beyond one trusted machine
1. Linux/container execution boundary (BLOCKED here: no Docker daemon).
2. Live GitHub run with a real token (NOT VERIFIED).
3. Verified clean-environment build (`docker build`) — BLOCKED here.
4. TLS / reverse proxy and token delivery for a non-loopback bind.
