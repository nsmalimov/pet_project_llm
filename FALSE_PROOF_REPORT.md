# FALSE_PROOF_REPORT — adversarial review of Proofline (2026-08-26)

Goal: make Proofline show **SUPPORTED** when it should not. Every scenario
below was checked against the code; the ones marked *vulnerable* were
reproduced with a test, fixed, and locked with a regression test. "Regression
test" names are in `internal/proof/falseproof_test.go` (unit, pure builder)
and `internal/engine/scenarios_test.go` (real engine runs on the fixture).

Invariant enforced after this pass:

> SUPPORTED = current persisted evidence satisfies the explicit policy of the
> claim **and** is bound to the exact source state (worktree HEAD per repo)
> the packet describes. Missing → INSUFFICIENT. Stale → STALE. Conflicting →
> CONTRADICTED, packet BLOCKED.

| # | scenario | before | risk | fix | regression test |
|---|---|---|---|---|---|
| 1 | Test passes but does not check the bug | baseline pass → CONTRADICTED (already) | — | kept; plus repro must be the **narrow** command when one exists (#11b) | `TestPacketRefusesReproductionWhenBaselinePasses`, `TestReproMustBeTheNarrowCommandWhenPresent` |
| 2 | Baseline reproduction missing | INSUFFICIENT (already) | — | kept; baseline on the wrong revision → STALE | `TestBaselineOnWrongRevisionIsStale`, scenario D |
| 3 | Test written together with the fix confirms the developer's wrong interpretation | **vulnerable**: author rewrites the failing assertion → suite passes → SUPPORTED (only a "medium risk" line) | false SUPPORTED on a fake fix | engine replays the **original** test files against the changed code (`original_tests_run` artifact); failing replay → CONTRADICTED; author-modified tests without replay → INSUFFICIENT | `TestAuthorModifiedTestsNeedOriginalReplay`, scenarios **B1** (inverted assertion) and **B2** (`t.Skip`) |
| 4 | Flaky test passed by chance | **vulnerable**: one green run counted | false SUPPORTED | narrow repro runs `RepeatRepro=2` times; disagreeing runs → CONTRADICTED ("flaky") | `TestFlakyRunsContradict` |
| 5 | Evidence belongs to an older commit | **vulnerable**: ordering by timestamp only | old evidence reused | every artifact records `source_shas` (worktree HEAD per repo) + `source_dirty`; claims accept only artifacts on the current state, others → STALE | `TestTestsAfterOldDiffDoNotCount`, scenario **F** |
| 6 | Code changed after verification | **vulnerable** (same as 5) | packet stays green after a later edit | uncommitted edit → dirty → everything STALE; committed edit → SHA mismatch → STALE | `TestDirtyWorktreeIsStale`, scenario F |
| 7 | Reviewer looked at a stale version | **vulnerable** (timestamp heuristic) | stale approval | review artifact bound to SHA; mismatch → STALE | `TestReviewMustPostdateDiffAndDifferInModel` |
| 8 | Reviewer found nothing → treated as correctness | wording implied correctness | over-trust | claim renamed "No counterexample found…", reason states absence ≠ proof; approve with empty `checked` → INSUFFICIENT; approve + counterexample or high finding → CONTRADICTED | `TestReviewerPolicies`, scenario **E** |
| 9 | Integration verification never ran | INSUFFICIENT (already) | — | kept as always-visible gap | scenario A/D |
| 10 | Downstream behaviour unchecked | INSUFFICIENT (already) | — | kept | — |
| 11 | Exit 0 but the needed test never executed (deleted, filtered, skipped) | **vulnerable**: command-level pass only | false SUPPORTED | `go test`/`pytest` run with `-v`; per-test results parsed (`tests`, `tests_parsed`); every test that failed on baseline must be observed **pass** now; zero tests executed → INSUFFICIENT | `TestExitZeroWithoutTestsIsNotVerification`, `TestReproTestMustRunAndPassAfterChange`, scenario B2 |
| 11b | Baseline "fails" because of a build error, not the bug | **vulnerable**: any non-zero exit counted as reproduction | false reproduction | baseline with a parsed runner and no failing test → INSUFFICIENT | `TestBaselineBuildErrorIsNotReproduction` |
| 12 | Agent summary claims more than evidence | summaries are never evidence (already) | — | root-cause is labelled agent-reported and only "supported" as a cross-check | `TestRootCauseNeedsFixInNamedFile` |
| 13 | Verification skipped | INSUFFICIENT (already) | — | kept | scenario D |
| 14 | Timeout mistaken for success | not a pass (exit -1) but not labelled | confusing | `timed_out` flag; after-change timeout → CONTRADICTED; baseline timeout → INSUFFICIENT | `TestTimeoutNeverLooksLikeSuccess` |
| 15 | Artifacts contradict each other | partially | — | any failing/timeout/disagreeing run on the current state → CONTRADICTED; packet lists `contradictions` | `TestFlakyRunsContradict`, scenario C |
| 16 | Rerun reuses stale evidence | **vulnerable** | old run counted | SHA binding (see 5) | scenario F |
| 17 | Claim leans on evidence of the wrong type | root cause used only hypothesis + timestamps | weak | root cause requires: named file exists ∧ diff touches it ∧ repro flipped on current state | `TestRootCauseNeedsFixInNamedFile` |
| 18 | Evidence without provenance | artifacts had run id + time only | unverifiable | artifacts carry `producer`, `worktree_root`, `source_shas`, `effective_command`, per-test results, exit code, full output | scenario matrix asserts every SUPPORTED claim has artifacts |
| 19 | Artifact from another workspace/task | **vulnerable**: not checked | cross-task contamination | artifacts with another `task_id` or `worktree_root` are ignored and counted as a gap | `TestForeignArtifactsIgnored` |
| 20 | Human sees an old packet after new code | **vulnerable** | decision on stale info | `PacketState` rebuilds on every read against the live worktree; content change → new version; UI shows STALE band and "decision refers to packet vN, current is vM" | scenario F, `TestProoflineVerticalSlice` (fingerprint stable when nothing changed) |

## What is still not closed (honest)

- **Semantic fake fix** where the author changes *production* code to make the
  original test pass without fixing the user-visible behaviour (e.g. special-
  casing the test input). Original-test replay does not catch it; only an
  independent challenger with a *new* test can. The reviewer may run tests now
  but cannot write files, so its counterexample stays a claim, not an executed
  artifact.
- **Flakiness** is only probed with 2 runs of the narrow command; the full
  suite runs once.
- **`not_checked`** is self-reported by the reviewer. An empty list is treated
  as unknown coverage, not as full coverage, but a lazy list is not detected.
- **Root cause** is a cross-check, never a causal proof.
- Test identity is verified for `go test` and `pytest` only; other runners
  fall back to command-level pass/fail and the packet says so
  ("individual test names not parseable").
- Legacy tasks created before source binding have artifacts without
  `source_shas`; their packets now read STALE/INSUFFICIENT, which is the
  correct default.
