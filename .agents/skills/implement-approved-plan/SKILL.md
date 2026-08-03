---
name: implement-approved-plan
description: Implement an approved repository-backed execution plan one boundary at a time while preserving existing work, running required verification, and updating plan progress. Use when the user asks to implement, continue, or resume an approved plan stored under docs/plans, especially when AGENTS.md and PLANS.md govern the workflow.
---

# Implement Approved Plan

Implement the next incomplete boundary from an approved plan without replacing or broadening it.

## Workflow

1. Read every applicable `AGENTS.md`, then read `PLANS.md` and the complete approved plan file.
2. Inspect `git status`, relevant diffs, and the implementation files named by the next incomplete boundary.
3. Preserve user changes and interrupted work. Reconcile partial edits with the approved plan; do not reset or discard them.
4. Confirm the plan is approved and select only its next incomplete implementation boundary.
5. Implement that boundary and no later boundary unless the user explicitly requests uninterrupted completion.
6. Run every verification command required for that boundary. Fix in-scope failures; record environmental failures exactly.
7. Update the plan's Progress, Discoveries, Decision Log, and Verification sections with factual results.
8. Mark a boundary complete only after its required verification passes.
9. Stop and report the completed boundary. Create a Git commit only when the user explicitly authorized committing.

## Guardrails

- Do not write a replacement plan.
- Do not silently alter approved scope, fixed specifications, dependency order, or acceptance criteria.
- Record implementation discoveries in the plan. If a discovery requires a material scope or behavior change, stop and request approval.
- Keep secrets out of plans, logs, events, errors, tests, and responses.
- Do not mark the plan Outcome complete until all acceptance criteria pass.
- When blocked, record the blocker and leave the boundary incomplete.
