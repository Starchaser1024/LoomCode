---
name: review
description: Structured git diff analysis for logic bugs, concurrency hazards, security issues, and regression risks
mode: inline
allowed_tools:
  - Bash
  - ReadFile
  - Grep
  - Glob
  - diff_summary
---

# Task

You are performing a rigorous, senior-engineer code review of the repository's current changes (or the target revision specified in `$ARGUMENTS`).

## Steps

1. Run `diff_summary` (pass `target` if `$ARGUMENTS` names a branch, commit, or `--staged`; otherwise inspect working tree changes) to get a structured breakdown of changed files, line counts, modified symbols, and automated risk signals.
2. Inspect the full diff via `git diff` / `git diff --staged` (using `Bash`), and read surrounding context in modified files with `ReadFile` and `Grep`—never judge a hunk in isolation without checking callers and invariants.
3. Evaluate every change across four dimensions:
   - **Correctness & Edge Cases**: nil/empty handling, off-by-one bounds, error propagation, partial-failure state, resource leaks.
   - **Concurrency & State**: lock ordering, data races on shared maps/slices, goroutine lifecycle/cancellation, channel blocking.
   - **Security & Boundaries**: path traversal, command/SQL injection, credential exposure, unsafe deserialization, permission bypasses.
   - **Regression & Test Coverage**: broken callers, public API compatibility, missing unit tests for new branches or bug fixes.
4. Produce a concise, actionable review report:
   - **Overview**: 1–2 sentences summarizing change scope and overall risk level (`Low` / `Medium` / `High`).
   - **Findings**: Grouped by severity (`[CRITICAL]`, `[HIGH]`, `[MEDIUM]`, `[NIT]`), each citing `file:line`, the concrete failure scenario, and a recommended fix.
   - **Verification Gaps**: Specific test cases that should be added or run before merging.

## Notes

- Do not modify any files during the review — report findings only unless the user explicitly asks to apply fixes.
- Skip purely stylistic nitpicks already enforced by `gofmt` / standard linters; focus on behavioral and architectural defects.
- If there are no staged or unstaged changes and no revision is specified in `$ARGUMENTS`, inspect `HEAD~1..HEAD` and note that you reviewed the latest commit.

$ARGUMENTS
