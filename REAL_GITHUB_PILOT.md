# REAL_GITHUB_PILOT — manual runbook for the live GitHub path (NOT VERIFIED here)

Everything below is implemented and tested against the local fake GitHub
(`internal/github/fakegh`, `TestGitHubPRLifecycle`). It has **not** been run
against api.github.com in this environment (no token). Status: NOT VERIFIED.

## Prerequisites
- A throwaway repository `OWNER/REPO` with a reproducible bug on the default
  branch and a PR branch with a fix (the `fixtures/reservations` content works:
  `scripts/fixture-repo.sh` then push).
- A fine-grained token with `contents:read`, `pull_requests:write`,
  `statuses:write` on that repository only. Never a classic org-wide token.
- A local mirror clone of the repository registered in Proofline.

## Steps
```bash
export GITHUB_TOKEN=github_pat_…            # never committed, never in prompts/artifacts
export PROOFLINE_GITHUB_WEBHOOK_SECRET=…    # random string, same in the GitHub webhook
export PROOFLINE_PUBLIC_URL=https://your-tunnel.example   # link back to packets
git clone https://github.com/OWNER/REPO ~/mirrors/REPO
./bin/orc repo add ~/mirrors/REPO --github OWNER/REPO
./bin/orc repo policy <repo_id> --file policy.json   # approved test/repro commands (+ optional HTTP check)
./bin/orc serve --addr 127.0.0.1:8080 --data ./.orchestrator
```
1. **Import the PR**: UI → New change case → GitHub pull request → owner/repo/number
   (or `POST /github/import`). Expect a verify-only case bound to base/head SHA.
2. **Verify**: watch the timeline; the baseline runs on the PR base, tests and the
   independent challenge on the head.
3. **Post**: on the packet, "Post status & comment to GitHub". Expect a commit
   status `proofline/change-assurance` = success **only** if SUPPORTED, else
   failure with the reason; a PR comment linking the packet; both recorded in
   the effects ledger (`GET /tasks/{id}/effects`). Posting again must not
   duplicate (already `done` for that packet fingerprint).
4. **Push a new commit** to the PR branch. With the webhook configured
   (`POST /github/webhook`, event pull_request/synchronize), expect a **new**
   case for the new head; the old packet stays v1 SUPPORTED for its own SHA;
   `orc github-status <old-case> --sha <new-sha>` prints NOT VERIFIED.
   Note: webhooks are refused in LOCAL_UNSAFE unless
   `PROOFLINE_ALLOW_UNSAFE_WEBHOOKS=1` (untrusted PR authors run code).
5. **Duplicate delivery**: redeliver the webhook from GitHub's UI → same case id,
   no second run, no second post.
6. **Revoke**: remove the token's access (or delete the repo) → `POST /github/import`
   again → open cases of that PR become FAILED "BLOCKED: GitHub access … revoked".

## What to record as evidence
- case ids and packet versions per head SHA; screenshots of the status on both
  commits; `effects.jsonl` content; `audit.jsonl` entries for `github.post`.
Mark the row "Real GitHub path" in PRIVATE_BETA_READINESS.md VERIFIED only
after all six steps behaved as described.
