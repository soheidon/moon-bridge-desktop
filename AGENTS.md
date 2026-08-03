# Development rules

## Planning

- Read this file, `PLANS.md`, and the active file under `docs/plans/` before complex work.
- Complex changes require a repository-backed plan approved by the user before implementation.
- Do not replace or broaden an approved plan. Record necessary deviations in its Decision Log and request approval when they change scope or behavior.
- Planning requests are read-only unless the user explicitly asks to create or update plan artifacts.

## Implementation

- Implement one planned commit boundary at a time.
- Inspect `git status` and existing diffs first. Preserve user changes and interrupted work; never discard them to obtain a clean tree.
- Follow the plan's dependency order and fixed specifications.
- Keep secrets write-only. Never place API keys in logs, events, errors, tests, or plan files.
- Do not create a Git commit unless the user explicitly asks for commits. A planned commit boundary is still the required unit of work.

## Verification and reporting

- Run the verification required by the active plan after each implementation boundary.
- Update Progress, Discoveries, Decision Log, and Verification in the plan with factual results.
- Do not mark work complete until all acceptance criteria pass.
- If blocked, record the blocker and leave the plan incomplete.

## Active plan

- The approved active plan is `docs/plans/plan-3s-crash-recovery.md`.
- `docs/plans/plan-3-traffic-analysis.md` remains approved but is not the active implementation plan; its live Codex Desktop feasibility experiment is still pending.
- `docs/plans/plan-3.md` is superseded by the traffic-analysis plan. Its launcher implementation remains in the worktree only as inactive legacy code until a later cleanup plan removes it.
- `docs/plans/plan-2b.md` remains the completed prior implementation plan with manual credential-backed acceptance still recorded as pending.
- Use `.agents/skills/implement-approved-plan/SKILL.md` when asked to implement an approved plan.
