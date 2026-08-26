# EVIDENCE_INVARIANTS — what a Proofline packet may and may not conclude

Scope: `internal/proof` (pure builder), `internal/engine` (artifact
production), `internal/sandbox` (execution boundary). Each invariant names
the test that enforces it. Statuses: VERIFIED = enforced by an automated
test in this repo; PARTIALLY VERIFIED = enforced by code, test covers part.

## Vocabulary

| concept | where | meaning |
|---|---|---|
| **evidence_kind** | `Artifact.Kind` | `baseline_run`, `test_run`, `original_tests_run`, `diff`, `review`, `root_cause`. Only the first five can support a claim; `root_cause` is a hypothesis. |
| **provenance** | `Artifact.Producer`, `RunID`, `WorktreeRoot`, `ExecMode`, `Effective`, `At` | which engine step / agent run produced it, in which workspace, under which execution mode, with which literal command. |
| **source_state** | `Artifact.SourceSHAs`, `SourceDirty`; `Packet.Source{Base,Head,Dirty}` | worktree HEAD per repo captured **before** the command ran and re-checked after; any movement or uncommitted change ⇒ `SourceDirty`. |
| **freshness** | `builder.current()`, `onBase()` | an artifact is current iff its source_state equals the packet's head state and neither is dirty; a baseline is valid iff its source_state equals the recorded base SHAs. Anything else is STALE. |
| **completeness** | `Artifact.Truncated`, `TestsParsed`, `TimedOut`, `Redacted` | whether the artifact holds the whole observation. Truncated or timed-out runs never support. |
| **verification_scope** | `Claim.Scope` | the repos/commands the evidence covers; a SUPPORTED claim says nothing outside its scope. |
| **claim_policy** | `Claim.Policy` (`proof.Policy*` constants) | the one-sentence rule that decides SUPPORTED, shipped inside the packet. |

## Invariants

**I1 — No artifact, no support.** A claim without a qualifying artifact is
INSUFFICIENT; free text from any agent is never evidence.
VERIFIED: `TestNoArtifactsMeansInsufficient`, `TestRootCauseNeedsFixInNamedFile`.

**I2 — Exact source binding.** Evidence supports only the source state it
was observed on. `test_run`/`review`/`diff` must match the current HEADs;
`baseline_run` must match the base SHAs. Otherwise STALE, never SUPPORTED.
VERIFIED: `TestTestsAfterOldDiffDoNotCount`, `TestBaselineOnWrongRevisionIsStale`,
`TestDirtyWorktreeIsStale`, `TestUnknownSourceStateCannotBeCurrent`,
scenario F, `TestVerifyOnlyModeAndStaleOnNewPush`.

**I3 — Source captured before, re-checked after.** The engine records HEADs
before running a command and marks the artifact dirty if they changed during
the run, so a race between a rerun/refresh and a running command can only
downgrade evidence, never mislabel it.
PARTIALLY VERIFIED: code in `engine.bindSource`; the proof-level effect is
covered by I2 tests; no test moves the worktree *during* a command.

**I4 — "Test output exists" ≠ "bug fixed".** Change verified requires: a
diff on the current state; all current runs passed, no timeout, repeats
agree; at least one test case executed; output complete; every test that
failed on the baseline observed passing now; and, if test files were
modified by the author, the original tests replayed against the change pass.
VERIFIED: `TestExitZeroWithoutTestsIsNotVerification`,
`TestReproTestMustRunAndPassAfterChange`, `TestFlakyRunsContradict`,
`TestTimeoutNeverLooksLikeSuccess`, `TestTruncatedOutputIsIncomplete`,
`TestAuthorModifiedTestsNeedOriginalReplay`, scenarios B1, B2, C.

**I5 — Reproduction is a failing test, not a failing command.** With a
parseable runner the baseline must contain a failing test case; a build
error or "no tests to run" is INSUFFICIENT; a passing baseline is
CONTRADICTED. When a narrow repro command exists, only it counts.
VERIFIED: `TestBaselineBuildErrorIsNotReproduction`,
`TestPacketRefusesReproductionWhenBaselinePasses`,
`TestReproMustBeTheNarrowCommandWhenPresent`, scenario D.

**I6 — Contradiction dominates.** Any contradicting artifact on the current
state (failing run, flaky pair, failing original-test replay, reviewer
counterexample / changes_requested / high finding) makes the claim
CONTRADICTED and the packet BLOCKED, regardless of other green artifacts.
VERIFIED: `TestFlakyRunsContradict`, `TestReviewerPolicies`, scenarios C, E.

**I7 — Independence is structural.** A review counts only if its model
differs from the author's, it was produced on the current diff without the
author's reasoning (prompt test `roles_test`), and it states what it checked.
"No findings" with no stated coverage is INSUFFICIENT.
VERIFIED: `TestReviewMustPostdateDiffAndDifferInModel`, `TestReviewerPolicies`,
`internal/roles` reviewer-prompt test.

**I8 — Provenance isolation.** Artifacts belonging to another task or
workspace are ignored and reported as a gap.
VERIFIED: `TestForeignArtifactsIgnored`.

**I9 — Completeness is explicit.** Output caps, redaction counts, timeouts
and unparseable runners are recorded on the artifact and shown; a capped
run cannot support a claim.
VERIFIED: `TestTruncatedOutputIsIncomplete`, `TestOutputCapAndRedaction`,
`TestSecretsInTestOutputAreRedactedInArtifacts`.

**I10 — Packets are immutable and reproducible.** A packet is appended only
when its fingerprint changes; every version stays readable; rebuilding from
artifacts after losing `packets.jsonl` yields the same fingerprint.
VERIFIED: `TestCrashAfterArtifactsBeforePacketRebuildsIdentically`,
`TestVerifyOnlyModeAndStaleOnNewPush` (v1 remains).

**I11 — Human decisions pin a packet version.** A verdict is stored with the
version the human saw; if the packet changed since, the verdict is refused
(409), never re-attached.
VERIFIED: `TestVerdictPinnedToViewedPacketVersion`.

**I12 — Unknown is never success.** Cancel, crash, timeout, policy violation
and dead executors leave the task INTERRUPTED / AWAITING_DECISION / FAILED
(BLOCKED) with no invented artifact; a resumed task must re-earn any
evidence for the state it is in.
VERIFIED: `TestCancelDuringVerificationKillsChildrenAndLeavesTaskRecoverable`,
`TestCrashDuringResearchIsInterruptedNotDone`,
`TestCrashDuringImplementationLeavesNoDiffArtifact`,
`TestCrashDuringTestExecutionRecoversAsInterrupted`,
`TestHostileRepoSymlinkEscapeBlocksTask`.

**I13 — Execution mode is part of the evidence.** Every artifact and packet
carries `exec_mode`; LOCAL_UNSAFE evidence is labelled as such and the API
exposes the enforced capabilities (`GET /system`).
PARTIALLY VERIFIED: fields set and asserted in
`TestSecretsInTestOutputAreRedactedInArtifacts`; the UI shows the mode only
in the raw artifact view.

## Known false-positive paths still open

- A production-code change that special-cases the reproduction input
  satisfies I4 and I5 (semantic fake fix). Needs an executed counterexample
  from the challenger.
- Flakiness is sampled (2 runs of the narrow command, 1 of the suite).
- Reviewer `checked`/`not_checked` are self-reported (I7 verifies presence,
  not truth).
- Test identity is parsed for Go and pytest only; other runners fall back to
  command-level pass/fail (stated on the claim).
