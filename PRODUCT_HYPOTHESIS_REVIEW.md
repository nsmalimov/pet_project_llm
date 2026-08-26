# PRODUCT_HYPOTHESIS_REVIEW — can a senior engineer trust the Change Case in 30–60 s?

Written after P0–P2 (2026-08-26). Deliberately critical. Based on the real
Claude run on `fixtures/reservations`, the six deterministic scenarios in
`docs/SCENARIOS.md`, and headless-browser screenshots of the Change Case.

## 1. Per-scenario self-evaluation

Questions: (1) what happened, (2) why this verdict, (3) what can be trusted,
(4) what cannot, (5) what is unknown, (6) where is the raw evidence.

| scenario | 30 s answer? | what works | what still costs time / trust |
|---|---|---|---|
| A valid fix (real Claude run + mock) | **yes** | band says SUPPORTED + "9 not verified"; four core claims each name the command, exit code, failing test name, commit; one click shows the raw `go test -v` output | the reader must notice that SUPPORTED ≠ safe: the gaps list (HTTP handler, concurrency) is long and easy to scroll past; root cause "supported" is a cross-check and the reader may over-read it |
| B1/B2 fake fix (assertion inverted / `t.Skip`) | **yes** | BLOCKED, first block: "the ORIGINAL tests fail against the changed code"; root cause INSUFFICIENT because the fix does not touch the named file; change lists `store_test.go` in amber | the reviewer (scripted here) still shows SUPPORTED — a reader could wonder why the "independent challenge" did not catch it. The answer (reviewer sees the diff but does not replay tests) is not on the screen |
| C regression | **yes** | BLOCKED, contradiction names the failing test `TestReserveDifferentDaysAndRooms`; workflow-paused block offers the engine decision | the reader sees two decisions (engine decision + human verdict). Which one matters is not obvious |
| D no tests | **yes** | INSUFFICIENT; "executed no test on the unchanged code"; change verified: "exit 0 but executed no test" | the review still reads SUPPORTED although nothing else is; the verdict is right but the green badge in a sea of amber is noise |
| E counterexample | **yes** | BLOCKED; counterexample is the very first line under the band and the top risk | the counterexample is a reviewer *claim*, not an executed test — the UI does not distinguish "reviewer said" from "a test showed" strongly enough |
| F stale | **yes** | STALE band, source `base → head` shows the new SHA; change verified / challenge STALE with "observed on X, worktree is at Y" | there is no button to re-verify the new state from the UI |

Verdict on the hypothesis: **for the "is this fix proven" question the screen
is faster than transcript + diff + CI**, mainly because of three things that
do not exist elsewhere: baseline-fail → after-pass on the same command with
test names, the original-tests replay, and STALE binding to the SHA. For the
"is this change safe to merge" question the screen is honest but not faster:
it says "not checked" a lot, and the engineer still has to think.

## 2. Proofline vs. the ordinary workflow (agent summary + diff + CI + logs)

### Where it saves real time
- **Reproduction proof.** CI tells you the suite is green now; it never tells
  you the suite was red before on the same command. Proofline shows both
  runs, the failing test name, and the SHA each was observed on.
- **Fake-fix detection.** Reviewing a diff that edits a test is easy to wave
  through. The original-tests replay turns it into a red block that cannot
  be waved through.
- **Staleness.** "Was this CI run for the commit I am looking at?" is a
  manual check on every PR. Here it is the verdict.
- **What the reviewer did NOT check** as a first-class list instead of a
  free-form comment.

### Where it adds information that did not exist
- per-test identity behind a green exit code (`-v` parsing);
- replay of original tests against the changed code;
- explicit source binding of every artifact;
- explicit "no integration/cross-service verification exists".

### Where it merely repackages
- the diff and the changed-files list (git already has them);
- agent runs, cost, tokens (nobody merges on that);
- the workflow decision panel (an ORC concept, not an assurance concept);
- the legacy `assumed…reviewed` confidence chain — superseded by claims, kept
  only in the raw tab;
- researcher "risks" (agent-reported, unverified) — mostly noise.

### Candidates for removal or demotion
1. The legacy evidence-chain table and `confidence` field (raw tab only, or
   delete).
2. Agent-run cost/token totals on the case page.
3. Researcher agent-reported risks unless something verifies them.
4. "Independent challenge SUPPORTED" as a green badge — rename the status
   to "no counterexample" visually so it never reads as a proof.

## 3. Where trust still leaks (highest first)
1. The reviewer's `checked` / `not_checked` lists are self-reported; a
   verbose reviewer looks more trustworthy than a careful one.
2. Root cause "supported" is a triangulation, not a proof; the badge is the
   same green as the test flip.
3. A semantic fake fix (production code special-casing the test input)
   passes every current policy. Only an executed counterexample would catch
   it; the challenger cannot write files.
4. Flakiness is probed twice, once for the full suite.
5. "SUPPORTED" with 9 gaps is still a green band. The band should carry the
   gap count as loudly as the verdict.
6. No integration runner: the two INSUFFICIENT rows are constant, so users
   will learn to ignore them — exactly the habituation the product exists to
   prevent.

## 4. What this iteration did NOT prove
- That real engineers decide faster — no user was observed. The 30-second
  claim is a self-evaluation by the author of the screen.
- That the policies generalise beyond Go/pytest test identity.
- That the reviewer adds real independent value on messy repositories; the
  one real run had it hand-trace tests because it could not run them.
