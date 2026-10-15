# Agent system

Primary is the engineering director. It returns a structured turn: objective, decisions, risks, and a dependency-aware task graph. Relo materializes that graph; OpenCode does not.

## Roles

| Role | Tier | Mutation | Job |
|---|---|---|---|
| primary | primary | no | Plan, delegate, integrate conclusions |
| head | tier1 | no | Subsystem coordination, compact reports upward |
| probe | tier2 | no | Reconnaissance, structured findings |
| worker / architect / debug / integration | tier1 | yes in Build mode | Implementation / design / fix / merge |
| test / reviewer | tier1 | no | Verify and independently review |

Task difficulty routes trivial/easy work to Tier 2, medium/hard work to Tier 1, and expert work to Primary; explicit difficulty-to-model mappings take precedence. Default difficulty is easy for probe/test/reviewer, medium for worker/integration/debug, hard for architect/head, and expert for Primary.

Plan and Review modes disable write/edit tools even for workers.

## Reports

Workers must return JSON:

```json
{
  "status": "completed",
  "summary": "…",
  "findings": [],
  "filesChanged": [],
  "verification": [{"command": "go test ./…", "status": "passed"}],
  "remainingRisks": [],
  "recommendedNextActions": []
}
```

Parents consume this report, not the worker transcript.

## Failure modes Relo blocks

- unbounded children (`maxHierarchyDepth`, concurrency caps)
- silent duplicate mutation (worktree isolation + conflict detection)
- full transcript propagation
- success without verification when `relo.autoVerify` is on
- unverified hypotheses becoming memory facts
- infinite fix/review loops (`maxRetries`)

Failed agents remain in the tree with status `failed` or `interrupted`. Retry is explicit.
