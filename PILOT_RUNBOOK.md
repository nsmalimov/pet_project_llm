# PILOT_RUNBOOK — one external user, one throwaway repository

Target: **trusted macOS host, SAFE_SANDBOX**. Linux is build-only
(LOCAL_UNSAFE) until a container boundary exists — do not pilot untrusted
repositories on Linux.

## 1. Prerequisites
Go ≥1.23, git, make, macOS with `/usr/bin/sandbox-exec`; `claude` CLI logged in
(real agents); optional GitHub fine-grained token scoped to the throwaway repo.

## 2. Start
```bash
make build
export PROOFLINE_SANDBOX=SAFE_SANDBOX
export PROOFLINE_REPOS_ROOT=$HOME/pilot-repos      # only repos under here
./bin/orc serve --addr 127.0.0.1:8080 --data ./.orchestrator
```
`GET /health`, `GET /ready` must return 200. The header shows `SAFE_SANDBOX`.

## 3. First user
```bash
./bin/orc auth init --workspace pilot --user alice     # token shown once
```
Open http://127.0.0.1:8080 → paste the token. Repositories & settings shows
workspace, role, tokens (issue/revoke), members (owner).
Non-loopback binds are refused until auth is configured.

## 4. Repository connection
```bash
git clone <throwaway> $HOME/pilot-repos/svc
./bin/orc repo add $HOME/pilot-repos/svc --github OWNER/svc
```

## 5. Policy (required in SAFE_SANDBOX)
`policy.json`:
```json
{"test_command":"go test ./...","repro_command":"go test -run TestBug ./...",
 "allowed_runners":["go"],"agent_write":true,
 "integration":{"start":"go run ./cmd/server","port":18080,"startup_seconds":60,
   "checks":[{"name":"health","method":"GET","path":"/healthz","expect_status":200}]}}
```
`./bin/orc repo policy <repo_id> --file policy.json`

## 6. Local Pilot run
UI → Cases → Load example (A/B/C/E) → Timeline → Proof packet → decision.
Or New change case → repository → goal/acceptance → Start verification
(real agents).

## 7. Real GitHub PR run
Follow `REAL_GITHUB_PILOT.md` (NOT VERIFIED live). Import PR → verify-only
case bound to base/head SHA.

## 8. Decision and status posting
Packet → Accept / Request changes / Reject (pinned to packet version).
Packet → "Post status & comment to GitHub" (needs `GITHUB_TOKEN`; success
only for SUPPORTED on the exact SHA; effects ledger prevents duplicates).

## 9. New push / stale
New PR head → new case; old packet stays historical. Local edits after
verification → packet STALE → "Re-verify current state".

## 10. Cancel / restart / recovery
Timeline → Cancel → case INTERRUPTED → Resume. `Ctrl-C` on the server
cancels running cases gracefully (they show interrupted, resumable).
Restart: `orc serve` recovers interrupted cases; nothing becomes done.

## 11. Backup / restore
```bash
./bin/orc backup pl-$(date +%F).tar --data ./.orchestrator
./bin/orc restore pl-2026-08-26.tar --data ./restored   # empty dir
```
Worktrees are not archived; restored cases show STALE/INSUFFICIENT until
re-verified.

## 12. Cleanup
`./bin/orc gc --older 168h` removes worktrees of finished cases (evidence stays).

## 13. Incident / rollback
- Case stuck: `GET /tasks/{id}/events`, `orc events <id>`; cancel from UI.
- Bad packet: never edit files; record a Reject with a note; re-verify.
- Roll back the binary: previous `bin/orc` with the same data dir (schema is
  additive JSON).
- Suspected secret leak: rotate the token (settings → issue new / revoke),
  `grep` the data dir; redaction is pattern-based.

## 14. Known unsafe limitations
See PRIVATE_BETA_READINESS.md "Remaining risk" column.
