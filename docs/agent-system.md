# Agent system

Primary is the engineering director. It returns a structured turn: objective, decisions, constraints, risks, and a dependency-aware task graph. Relo materializes that graph; OpenCode does not.

## Roles

| Role | Tier | Mutation | Job |
|---|---|---|---|
| primary | primary | no | Plan, delegate, integrate conclusions |
| head | tier1 | no | Subsystem coordination, compact reports upward |
| probe | tier2 | no | Reconnaissance, structured findings |
| worker / architect / debug / integration | tier1 | yes in Build mode | Implementation / design / fix / merge |
| test / reviewer | tier1 | no | Verify and independently review |

Task difficulty routes trivial/easy work to Tier 2, medium/complex/hard work to Tier 1, and expert work to Primary; explicit difficulty-to-model mappings take precedence. `complex` sits between medium and hard (a larger change that still follows existing patterns); without its own mapping it uses the hard model. The Primary rates each task by what its agent must still work out, not by the feature: implementing an architect's design is usually medium or complex, and when unsure it rates lower, since a failed task can be retried higher. Hard and expert tasks always get the automatic review, trivial ones never, and the rest only for a large diff. Default difficulty is easy for probe/test/reviewer, medium for worker/integration/debug, hard for architect/head, and expert for Primary.

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
  "recommendedNextActions": [],
  "handoff": "",
  "question": ""
}
```

Parents consume this report, not the worker transcript.

- `handoff` is what tasks that build on this one need (names, signatures, file paths, decisions). It is the one part of a report that is not compressed: dependent tasks receive it whole, up to 800 characters.
- An architect's report is the spec its dependent tasks build from, so its `summary` keeps up to 24,000 characters; every other role is held to `relo.report.maxChars` (default 1,200). The Primary and the project memory still see the architect's report at the shorter limit, and only the tasks that depend on it receive the full text.
- `status: "blocked"` with a `question` is for a decision that is not the agent's to make. The task ends without a retry, the agent shows "Needs a decision", and the Primary reads the question in the report and answers it in a replacement task or asks the user.

## Planning

Planning is not capped: a session may plan any number of tasks, and a task with the same role and title as one already planned is dropped as a repeat unless the earlier one failed or was canceled. The Primary prompt tells the planner to keep a task to one subsystem and about 25 files, to give every task the files it edits in `paths`, to let only one task edit a shared ledger, changelog, status or registry file, and to produce tracked build outputs (dll, pdb, generated assets) from a single integration task after the source tasks finish.

## Worker start

A worker's first message is its task in full: title, description, acceptance criteria and the files in scope, last in the message. The context pack keeps only the title. A retried task also gets a "Previous attempt failed" item with the reason the last attempt gave.

## Constraints

Primary reads the user's request and the context and returns `constraints`: short, checkable rules every sub agent must follow, including ones the user did not spell out. A non-empty list replaces the session's list; an empty one leaves it unchanged. Core stores the list in the session, shows it to the Primary each turn, and pins it in the context of every sub agent except probes.

## Failure modes Relo blocks

- unbounded children (`maxHierarchyDepth`, concurrency caps)
- silent duplicate mutation (worktree isolation + conflict detection)
- full transcript propagation
- success without verification when `relo.autoVerify` is on
- unverified hypotheses becoming memory facts
- infinite fix/review loops (`maxRetries`)

Failed agents remain in the tree with status `failed` or `interrupted`. Retry is explicit.
