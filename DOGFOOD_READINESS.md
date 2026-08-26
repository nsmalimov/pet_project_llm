# DOGFOOD_READINESS — why five skeptical staff engineers might say "I'd rather read the diff and CI myself"

Written 2026-08-26 after P0–P4. Each reason is concrete, grouped by cause.
The TOP-5 section says what was fixed now and what was not.

## Trust
1. **"SUPPORTED" still renders as a green band.** With 9 gaps under it, a
   green band trains people to stop reading. *(fixed now: the band carries
   "N NOT VERIFIED" inside the verdict, green desaturated)*
2. **Reviewer coverage is self-reported.** `checked`/`not_checked` come from
   the same LLM whose judgment is in question; a verbose reviewer looks safer.
3. **Root cause "supported" is a triangulation**, but wears the same badge as
   an executed test flip. *(reason text says "cross-check, not a causal
   proof"; the badge does not)*
4. **The challenger cannot execute its counterexamples** (no Write; in the
   real run it says so explicitly). A counterexample is a claim, not an
   artifact.
5. The engine committed the agent's change with `--no-verify` under the
   identity `orc` — nobody signed it.

## Correctness
6. Test identity is verified for Go and pytest only; npm/make fall back to
   command-level pass/fail silently unless the reader opens the artifact.
7. Flakiness: two runs of the narrow repro, one run of the full suite.
8. A production-code special-case that satisfies the original test passes
   every policy (semantic fake fix).
9. Multi-repo tasks: baseline/verification per repo, no cross-repo test —
   the packet says INSUFFICIENT but cannot say *what* to run.
10. The reviewer is routed to "a different model", not a different vendor;
    shared blind spots are likely.

## UX / speed
11. Two decision panels can appear at once (engine decision + human
    verdict); which one unblocks what is not obvious.
12. The gaps list can be 9+ lines of reviewer prose; no ranking of gaps by
    what would change the verdict.
13. No "re-verify on the current state" button when STALE; it is a CLI
    action.
14. No diff viewer — the diff is a `<pre>` block.
15. Case index has no filters; fine at 10 cases, useless at 200.

## Workflow friction
16. Creating a case needs a local repo path and a repro command; no GitHub
    import (no token in this environment — see blocker).
17. Cases live in a local data dir; a URL is only shareable inside the
    machine.
18. Human decision has no consequence (no merge, no label, no status post
    unless the CLI is run with a token).

## Evidence quality / noise
19. Constant INSUFFICIENT rows (integration, cross-service) on every case
    will be tuned out within a week — the product's own habituation risk.
20. Researcher "agent-reported risks" are almost always noise ("none
    significant").
21. The legacy `assumed…reviewed` confidence chain still exists in the raw
    tab and in the API; two truth systems confuse.

## Missing context / GitHub / agent behaviour
22. No link from a claim to the PR/issue that motivated the change.
23. GitHub posting is implemented but never executed against GitHub (no
    token) — untested in the real world.
24. The reviewer was declined `git diff <base>` in the real run (allow-list
    mismatch on the command shape) and had to read the tree; cheap to fix
    but shows the tool allow-list is brittle.
25. The developer role runs with blanket Bash inside the worktree; a
    skeptical engineer will ask what else it ran. Commands are not captured
    as artifacts.

## TOP-5 most dangerous, and what was done now

| # | reason | done now | remaining |
|---|---|---|---|
| 1 | green band + long gap list → habituation (#1, #19) | verdict band shows "N NOT VERIFIED" inside the verdict; supported green desaturated; contradictions always first | gaps are not ranked by impact |
| 2 | fake-fix / stale evidence / silent test skip (#8, FALSE_PROOF 3/5/11) | original-test replay, SHA binding, per-test identity, flaky guard, all with regression tests | semantic fake fix needs an executing challenger |
| 3 | reviewer counterexample is a claim, not an artifact (#4) | counterexample → CONTRADICTED and top risk; reviewer may run `go test`/`pytest` | reviewer cannot write a test; needs a sandboxed scratch dir |
| 4 | "no findings" read as proof (#2) | approve without `checked` → INSUFFICIENT; claim renamed "No counterexample found…" | coverage still self-reported |
| 5 | GitHub result could be a fake green (#23) | `BuildStatus` returns success only for SUPPORTED on the exact SHA; anything else → failure "NOT VERIFIED"; tested with a push of commit B | never posted to real GitHub — blocker: no token |
