# Execution plans

Execution plans are living repository documents for complex work. Store them under `docs/plans/`.

Every plan must contain:

- Status and approval state
- Purpose
- Scope
- Non-goals and fixed specifications
- Current state
- Implementation boundaries and dependency order
- Progress with dated factual notes
- Discoveries
- Decision Log
- Verification commands and results
- Acceptance criteria
- Outcome

## Rules

1. Inspect the repository before writing a plan.
2. Make the plan concrete enough for another agent to continue without conversation history.
3. Treat approved scope and fixed specifications as authoritative.
4. Record discoveries instead of silently rewriting earlier decisions.
5. Implement only the next incomplete boundary unless the user explicitly requests uninterrupted completion.
6. Update Progress after work and tests, not before.
7. Record exact verification results, including failures and environmental limitations.
8. Keep Outcome incomplete until every acceptance criterion passes.
