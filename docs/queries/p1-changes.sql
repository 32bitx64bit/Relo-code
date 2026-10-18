-- P1 baseline query. Run against a Relo database:
--   sqlite3 ~/.relo/relo.db < docs/queries/p1-changes.sql
--
-- It reproduces the §1.1 evidence table in docs/design/changes-conflicts.md from the `changes`, `agents`, `tasks`
-- and `events` tables. `changes.payload` is the change JSON; `agents.payload` holds `createdAt` (RFC3339) and
-- `endedAt` (unix ms). Decision time is the first `changes.updated` event whose payload array carries the change
-- with a non-proposed status. Times are unix seconds unless the column says otherwise.
--
-- P1 fields: `baseRev`, `needs`, `lineage`, `landed`, `integration`.
-- integration.kind, integration.hunks and landed are new in P1; rows written before it have none.

-- 1. Change status: accepted / rejected / proposed.
SELECT json_extract(payload, '$.status') AS status, COUNT(*) AS n
FROM changes GROUP BY status ORDER BY n DESC;

-- 2. Changes ever flagged as conflicted (legacy text or P1 integration state).
SELECT COUNT(*) AS conflicts,
       COUNT(DISTINCT session_id) AS sessions,
       COUNT(DISTINCT json_extract(payload, '$.agentId')) AS agents
FROM changes
WHERE COALESCE(json_extract(payload, '$.conflict'), '') <> ''
   OR json_extract(payload, '$.integration.state') = 'conflicted';

-- 2b. The same, by final status.
SELECT json_extract(payload, '$.status') AS status, COUNT(*) AS n
FROM changes
WHERE COALESCE(json_extract(payload, '$.conflict'), '') <> ''
   OR json_extract(payload, '$.integration.state') = 'conflicted'
GROUP BY status;

-- 3. Distinct stored conflict reasons and their kinds.
SELECT COALESCE(json_extract(payload, '$.conflict'), '') AS reason,
       COALESCE(json_extract(payload, '$.integration.kind'), 'legacy') AS kind,
       COUNT(*) AS n
FROM changes
WHERE COALESCE(json_extract(payload, '$.conflict'), '') <> ''
GROUP BY reason, kind;

-- 4. Legacy fields the design measured: BaseBlob, Superseded, Resolving.
SELECT COUNT(*) AS total,
       SUM(COALESCE(json_extract(payload, '$.baseBlob'), '') <> '') AS base_blob_set,
       SUM(COALESCE(json_extract(payload, '$.superseded'), 0) <> 0) AS superseded,
       SUM(COALESCE(json_extract(payload, '$.resolving'), 0) <> 0) AS resolving
FROM changes;

-- 5. Changes on a path touched by more than one agent in the same session.
SELECT COUNT(*) AS changes_on_shared_paths
FROM changes c
WHERE EXISTS (
  SELECT 1 FROM changes o
  WHERE o.session_id = c.session_id
    AND json_extract(o.payload, '$.path') = json_extract(c.payload, '$.path')
    AND json_extract(o.payload, '$.agentId') <> json_extract(c.payload, '$.agentId'));

-- 5b. Multi-agent (session, path) pairs.
SELECT COUNT(*) AS multi_agent_pairs FROM (
  SELECT session_id, json_extract(payload, '$.path') AS path
  FROM changes
  GROUP BY session_id, path
  HAVING COUNT(DISTINCT json_extract(payload, '$.agentId')) > 1);

-- Agent cut time and change decision time, used by the pair queries below.
CREATE TEMP VIEW IF NOT EXISTS p1_times AS
SELECT c.id, c.session_id,
       json_extract(c.payload, '$.agentId') AS agent_id,
       json_extract(c.payload, '$.path') AS path,
       COALESCE(json_extract(c.payload, '$.status'), '') AS status,
       COALESCE(strftime('%s', json_extract(a.payload, '$.createdAt')), 0) AS cut_at,
       COALESCE(
         (SELECT MIN(e.created_at) FROM events e, json_each(e.payload) j
          WHERE e.session_id = c.session_id AND e.name = 'changes.updated'
            AND json_extract(j.value, '$.id') = c.id
            AND json_extract(j.value, '$.status') <> 'proposed'),
         c.updated_at) AS decided_at
FROM changes c
LEFT JOIN agents a ON a.id = json_extract(c.payload, '$.agentId');

-- 6. Same-path later-vs-earlier pairs where the earlier change was already decided when the later agent was cut.
WITH pairs AS (
  SELECT e.id AS earlier, l.id AS later, e.status AS earlier_status
  FROM p1_times e JOIN p1_times l
    ON e.session_id = l.session_id AND e.path = l.path AND e.agent_id <> l.agent_id
   AND e.cut_at < l.cut_at
)
SELECT CASE WHEN earlier_status = 'proposed' THEN 'pending at cut' ELSE 'decided at cut' END AS bucket,
       COUNT(*) AS pairs,
       SUM(CASE WHEN COALESCE(json_extract(lc.payload, '$.conflict'), '') <> ''
                  OR json_extract(lc.payload, '$.integration.state') = 'conflicted'
                THEN 1 ELSE 0 END) AS conflicts
FROM pairs JOIN changes lc ON lc.id = pairs.later
GROUP BY bucket;

-- 7. "Exposed": another agent's change on the same path was accepted between this agent's cut and this change's
-- decision. Counts conflicts in the exposed and unexposed sets.
WITH accepts AS (
  SELECT session_id, json_extract(payload, '$.path') AS path,
         json_extract(payload, '$.agentId') AS agent_id, decided_at AS accepted_at
  FROM p1_times WHERE status = 'accepted'
)
SELECT CASE WHEN EXISTS (
         SELECT 1 FROM accepts a
         WHERE a.session_id = t.session_id AND a.path = t.path AND a.agent_id <> t.agent_id
           AND a.accepted_at > t.cut_at AND a.accepted_at < t.decided_at
       ) THEN 'exposed' ELSE 'not exposed' END AS bucket,
       COUNT(*) AS changes,
       SUM(CASE WHEN COALESCE(json_extract(c.payload, '$.conflict'), '') <> '' THEN 1 ELSE 0 END) AS conflicts
FROM p1_times t JOIN changes c ON c.id = t.id
GROUP BY bucket;

-- 8. Other agents' pending changes at the moment an agent was cut (raw counts; median = the middle row ordered by
-- n, p90 = the row at 90% of the ordered rank).
SELECT cut_at, cut_agent, COUNT(*) AS pending_other_agents
FROM (
  SELECT DISTINCT t.cut_at, t.agent_id AS cut_agent, other.id
  FROM p1_times t JOIN p1_times other
    ON other.session_id = t.session_id AND other.agent_id <> t.agent_id
   AND other.cut_at < t.cut_at AND other.decided_at > t.cut_at
)
GROUP BY cut_at, cut_agent ORDER BY pending_other_agents;

-- 8b. Pending changes at cut that shared a path with the new agent.
SELECT COUNT(*) AS pending_sharing_path FROM (
  SELECT DISTINCT t.agent_id, other.id
  FROM p1_times t JOIN p1_times other
    ON other.session_id = t.session_id AND other.agent_id <> t.agent_id
   AND other.cut_at < t.cut_at AND other.decided_at > t.cut_at AND other.path = t.path
);

-- 9. Agent run time (seconds, agents that produced changes).
WITH runs AS (
  SELECT (json_extract(a.payload, '$.endedAt') - 1000 * strftime('%s', json_extract(a.payload, '$.createdAt'))) AS run_ms
  FROM agents a
  WHERE a.id IN (SELECT json_extract(payload, '$.agentId') FROM changes)
    AND COALESCE(json_extract(a.payload, '$.endedAt'), 0) > 0
)
SELECT COUNT(*) AS agents,
       CAST(AVG(run_ms) / 1000 AS INT) AS mean_s,
       (SELECT CAST(run_ms / 1000 AS INT) FROM runs ORDER BY run_ms LIMIT 1 OFFSET (SELECT (COUNT(*) - 1) / 2 FROM runs)) AS median_s,
       (SELECT CAST(run_ms / 1000 AS INT) FROM runs ORDER BY run_ms LIMIT 1 OFFSET (SELECT COUNT(*) * 9 / 10 FROM runs)) AS p90_s
FROM runs;

-- 10. Finish to accept and cut to accept (seconds). The accept time is the change's decision time.
SELECT CAST((t.decided_at - json_extract(a.payload, '$.endedAt') / 1000) AS INT) AS finish_to_accept_s,
       CAST((t.decided_at - t.cut_at) AS INT) AS cut_to_accept_s
FROM p1_times t JOIN agents a ON a.id = t.agent_id
WHERE t.status = 'accepted' AND COALESCE(json_extract(a.payload, '$.endedAt'), 0) > 0
ORDER BY finish_to_accept_s;

-- 11. Tasks and dependency coverage: tasks with Depends; changes whose task has Depends; of those, changes whose
-- dependency task also produced changes.
SELECT COUNT(*) AS tasks,
       SUM(CASE WHEN json_array_length(COALESCE(json_extract(payload, '$.dependencies'), '[]')) > 0 THEN 1 ELSE 0 END) AS tasks_with_depends
FROM tasks;

SELECT COUNT(*) AS changes_with_depends FROM changes c
JOIN tasks t ON t.id = json_extract(c.payload, '$.taskId')
WHERE json_array_length(COALESCE(json_extract(t.payload, '$.dependencies'), '[]')) > 0;

SELECT COUNT(*) AS changes_whose_dependency_produced_changes
FROM changes c
JOIN tasks t ON t.id = json_extract(c.payload, '$.taskId'),
     json_each(t.payload, '$.dependencies') d
WHERE EXISTS (SELECT 1 FROM changes dc
              JOIN tasks dt ON dt.id = json_extract(dc.payload, '$.taskId')
              WHERE dt.id = d.value AND COALESCE(json_extract(dt.payload, '$.agentId'), '') <> '')
  AND json_array_length(COALESCE(json_extract(t.payload, '$.dependencies'), '[]')) > 0;

-- 12. autoApply default is code, not data: extension/package.json:494.
