# FABLE_HANDOFF — Proofline foundation, state at 2026-08-26

Language used below: **VERIFIED** (automated test in this repo), **PARTIALLY
VERIFIED** (code + partial test, or verified once by hand), **NOT VERIFIED**,
**BLOCKED** (cannot be done in this environment). Nothing here is
production-ready; see §9.

## 1. Architecture now

Single Go binary `orc`, stdlib only, modular monolith:

```
cmd/orc ─► api (auth middleware, routes, webhook) ─► engine ─► roles/router/executor/gitws/verify/proof/store/repos
                                                              │
                             sandbox (execution boundary) ◄───┘   domain (pure types)
```

- `internal/sandbox` — the execution boundary: canonical paths, worktree
  scanner, argv command policy, constructed environment, process-group kill,
  output/artifact caps, secret redaction, macOS `sandbox-exec` profiles.
  Modes `SAFE_SANDBOX` / `LOCAL_UNSAFE` (`GET /system`, packet `exec_mode`).
- `internal/gitws` — one git worktree per repo per task, hardened git
  (`core.hooksPath=/dev/null`, no fsmonitor/pager/editor/credential helpers,
  empty HOME, `GIT_CONFIG_NOSYSTEM`), scan after checkout and before verify,
  `Fetch`/`ApplyHead`/`Heads`/`WithOriginalFiles`.
- `internal/engine` — re-planning loop (understand → implement → verify →
  review), baseline reproduction, artifact production with source binding,
  packet snapshots, verdicts, idempotent creation, GitHub flow with an
  external-effects ledger, verify-only mode (`HeadRef`, `PinnedBase`).
- `internal/proof` — pure packet builder; the only place claims are decided.
- `internal/store` — file store: `task.json` (CAS via `Version`), JSONL
  append logs, per-task flock lease, `idempotency.json`, `effects.jsonl`,
  `packets.jsonl`, `verdicts.jsonl`, `artifacts.jsonl`.
- `internal/auth` — workspaces, users, memberships, tokens, permission matrix.
- `internal/repos` — repository registry (IDs, canonical paths, workspace,
  GitHub link).
- `internal/github` — PR fetch, webhook verification/parsing, status/comment
  builders, client; `fakegh` local GitHub for tests.
- `internal/api/ui.html` — one embedded page; reads the API only.

## 2. Security model

| control | mode | status |
|---|---|---|
| test/repro commands: argv split, no shell, runner allowlist, `-exec/-toolexec/python -c` rejected | both | VERIFIED `sandbox_test`, `security_test` |
| constructed env for commands (no host secrets, HOME/TMPDIR/GOCACHE inside workspace) | both | VERIFIED `TestHostEnvironmentNotInherited` |
| process group kill on timeout/cancel incl. background children | both | VERIFIED `TestBackgroundChildrenAreKilledOnTimeout`, `TestCancelDuringVerification…` |
| output cap + tail, artifact cap, diff cap, worktree size cap | both | VERIFIED (`TestOutputCapAndRedaction`, scan size test) |
| secret redaction before persisting artifacts/events/runs | both | VERIFIED `TestSecretsInTestOutputAreRedactedInArtifacts` — pattern-based, not complete |
| canonical repo paths; repo inside workspace rejected; traversal rejected; symlink escape / submodule / nested repo → task BLOCKED | both | VERIFIED `TestHostileRepoSymlinkEscapeBlocksTask`, `TestAgentPlantedSymlinkBlocksBeforeVerification`, `TestPathTraversalAndSymlinkEscape` |
| git hooks / fsmonitor / credential helpers neutralised | both | PARTIALLY VERIFIED (flags set; no test with a malicious hook) |
| agent env allowlist (no AWS_*/GITHUB_TOKEN etc. reach the CLI) | both | PARTIALLY VERIFIED (code; live probe could not execute `printenv` because it is not in the Bash allowlist) |
| developer Bash allowlist (runners + read-only git), no WebFetch/WebSearch | both | NOT VERIFIED beyond configuration; `Bash(go:*)` still permits `go run` of repo code — in LOCAL_UNSAFE that is host access |
| filesystem isolation for commands (deny ~/.ssh, ~/.aws, …; writes only in workspace) and network deny | SAFE_SANDBOX (macOS) | VERIFIED `TestSafeSandboxDeniesNetworkAndHostSecrets`, `TestSafeSandboxCanRunGoTests` |
| filesystem isolation for the Claude CLI (Read tool EPERM outside allowed roots, network allowed) | SAFE_SANDBOX (macOS) | PARTIALLY VERIFIED — live probe `TestLiveClaudeUnderSafeSandbox` passed once by hand (PROOFLINE_LIVE=1); deny-list of secret dirs + allow-list of toolchain dirs, not a full allow-list |
| Linux sandbox (bwrap/namespaces), rlimits, docker | any | BLOCKED here: `SAFE_SANDBOX` refuses to start on non-macOS |
| repository IDs; SAFE mode refuses raw paths and needs `PROOFLINE_REPOS_ROOT` | both | VERIFIED `repos` via engine tests (unsafe path); SAFE-mode refusal NOT VERIFIED by test |
| API: bearer tokens, permission matrix, workspace scoping, 404 across tenants, revoked membership | auth configured | VERIFIED `internal/api/authz_test.go` |
| serve refuses non-loopback bind without auth | — | NOT VERIFIED (code only) |
| webhook HMAC + delivery-id idempotency | — | VERIFIED `TestWebhookSignatureAndParsing`, `TestGitHubPRLifecycle` |

## 3. State invariants (P2)

- `task.json` is CAS: `SaveTask` fails with `ErrConflict` on version
  mismatch — VERIFIED `TestSaveTaskIsCompareAndSwap`.
- One executor per task across goroutines and processes (in-memory map +
  flock lease; a dead worker's lock file does not block) — VERIFIED
  `TestTwoWorkersOneExecutes`, `TestStaleLeaseFromDeadWorkerDoesNotBlockRestart`.
- Decisions resolve exactly once; stale/duplicate resolves are rejected —
  VERIFIED `TestResolveDecisionTwiceConcurrently`, `TestResolveDecisionGuardsTaskStatus`.
- Creation with an idempotency key is replay-safe under concurrency —
  VERIFIED `TestIdempotentCreation`; a reservation whose creator crashed
  surfaces as an error after 10 s (never a second task).
- Human verdict is refused if the packet version changed — VERIFIED
  `TestVerdictPinnedToViewedPacketVersion`.
- Cancel kills the process group, leaves the task INTERRUPTED with a resume
  point, records no pass — VERIFIED.
- External effects: intent persisted before the POST; pending/unknown intent
  on retry → refuse (`ErrEffectUnknown`) — VERIFIED `TestGitHubPRLifecycle`.
- Known gap: event `Seq` is a per-process lazy counter (documented since v0);
  packet version numbering relies on the per-task lease + JSONL append and is
  NOT protected by CAS when two processes call `PacketState` concurrently
  (SUSPECTED duplicate version numbers; see §7).

## 4. Evidence invariants
See `EVIDENCE_INVARIANTS.md` (I1–I13, each mapped to tests) and
`FALSE_PROOF_REPORT.md`.

## 5. GitHub invariants (P4)
- A packet describes exactly one head SHA; `BuildStatus` returns `success`
  only for SUPPORTED on that exact SHA and a clean tree — VERIFIED.
- New head → new ChangeCase; old case/packet untouched — VERIFIED.
- Duplicate deliveries (same ID) and same-head re-deliveries → one case —
  VERIFIED.
- Revocation (401/403/404) → open cases FAILED with "BLOCKED: … revoked" —
  VERIFIED against `fakegh`. Against real GitHub: **BLOCKED** (no token).
- PR base is pinned (`PinnedBase`) so the baseline runs on the PR base, not
  the mirror's HEAD — VERIFIED in `TestGitHubPRLifecycle`.
- Repro command for PR cases is not derived from anywhere yet (the test sets
  it by hand) — NOT VERIFIED path; see §8.

## 6. What is actually tested
`go test -race ./...` (all packages): sandbox adversarial tests (incl. two
macOS-only SAFE_SANDBOX probes), engine security/concurrency/lifecycle/
GitHub/scenario-matrix tests, proof false-proof regressions, API
cross-tenant tests, store CAS. One live Claude probe behind `PROOFLINE_LIVE=1`.

## 7. What remains unsafe (be explicit)
1. LOCAL_UNSAFE is the default; agents and test commands reach the host. The
   warning prints on every command and on `GET /system`.
2. No Linux sandbox; SAFE_SANDBOX is macOS `sandbox-exec` (deprecated by
   Apple but functional).
3. Agent Bash allowlist admits `go run`/`npm run`/`make` — arbitrary code
   from the repository under the agent's identity; only SAFE mode confines it.
4. Redaction is regex-based; unknown secret shapes leak.
5. The reviewer's `git diff <sha>` was declined once by the CLI allow-list
   shape in a real run; reviewer coverage is self-reported.
6. `PacketState` under concurrent processes may append duplicate packet
   version numbers (no CAS on `packets.jsonl`).
7. Engine tests share one Go build cache (`PROOFLINE_SHARED_CACHE`) —
   tests only.

## 8. What remains stubbed
- Integration / cross-service claims: no runner; always INSUFFICIENT.
- PR import does not know the repo's repro/test policy; `--repro-cmd` per
  repository is not stored.
- GitHub posting/import never executed against real GitHub.
- UI does not show workspaces, auth, effects, or exec mode prominently.
- `orc auth` has no token rotation UX beyond issue/revoke functions.

## 9. P0 blockers before an external pilot
1. Run the full flow against real GitHub with a fine-grained token on a
   throwaway repo (statuses, comment, webhook, revocation).
2. Decide the hosting OS: Linux needs a sandbox implementation (bwrap or
   containers) before any untrusted repository is accepted.
3. Store repository policy (test/repro commands, roots) with the repo
   registration; PR-created cases currently have no repro command.
4. Fix §7.6 (packet version CAS) or serialise `PacketState` per task.
5. TLS/reverse proxy and token delivery UX (tokens are printed once).

## 10. Mechanical work suitable for Sonnet
- UI: workspace switcher, token entry, show `exec_mode` warning banner,
  effects tab, "re-verify" button (calls `POST /github/import`), gaps ranking.
- CLI polish: `orc repo link`, `orc auth token issue/revoke`, `orc pr import`.
- README/NEXT prose, `docs/SCENARIOS.md` regeneration in CI.
- More runner parsers for test identity (jest, cargo, JUnit XML).
- CI workflow: vet, race tests, `PROOFLINE_WRITE_MATRIX` diff check.

## 11. Work that should still use a stronger model
- Linux execution boundary (bwrap/seccomp/cgroups) and its adversarial tests.
- Semantic fake-fix defence: an executing challenger with a scratch dir.
- Packet/version CAS and event Seq across processes; SQLite store swap.
- Repository policy model (allowed commands per repo, approval flow).
- Any change to `internal/proof` policies.

## 12. Explicit things NOT to refactor
- `internal/proof.Build` as a pure function and its policy constants.
- Artifact source binding (`bindSource`, `Heads`, `current()/onBase()`).
- Original-tests replay (`WithOriginalFiles`, `replayOriginalTests`).
- CAS `SaveTask`, idempotency claim, effects ledger semantics.
- Reviewer isolation (prompt without author reasoning; different model).
- The scenario matrix test and false-proof regression tests — extend, never
  weaken.
