import { readFileSync } from "node:fs";
import { createServer } from "node:net";
import assert from "node:assert/strict";
import { test } from "node:test";
import { decodeFrame, encodeFrame } from "./client";
import { Events, emptyState, reduce, routingFrom, type CacheInfo } from "./protocol";

test("encode/decode ipc frames", () => {
  const line = encodeFrame("session.create", { mode: "build" }, "1");
  const f = decodeFrame(line);
  assert.equal(f.kind, "req");
  assert.equal(f.method, "session.create");
  assert.equal(f.v, 1);
});

test("reduce agent and task events", () => {
  let s = emptyState();
  s = reduce(s, "agent.created", { id: "a1", title: "Primary", role: "primary", status: "running" });
  s = reduce(s, "task.created", { id: "t1", title: "Probe", status: "queued" });
  s = reduce(s, "chat.message", { id: "m1", role: "user", text: "hello", createdAt: "now" });
  assert.equal(s.agents.length, 1);
  assert.equal(s.tasks[0].title, "Probe");
  assert.equal(s.messages[0].text, "hello");
  s = reduce(s, "agent.activity", { id: "a1", activity: "streaming", status: "running" });
  assert.equal(s.agents[0].activity, "streaming");
});

test("chat deltas create per-agent streaming messages and finalize", () => {
  let state = emptyState();
  state = reduce(state, "chat.delta", { agentId: "a1", delta: "hel" });
  state = reduce(state, "chat.delta", { agentId: "a2", delta: "other" });
  state = reduce(state, "chat.delta", { agentId: "a1", delta: "lo" });
  assert.equal(state.messages[0].text, "hello");
  assert.equal(state.messages[1].text, "other");
  state = reduce(state, "chat.message", { id: "m1", role: "assistant", agentId: "a1", text: "hello!", createdAt: "now" });
  assert.equal(state.messages.length, 2);
  assert.equal(state.messages[0].streaming, false);
  assert.equal(state.messages[0].text, "hello!");
});

test("cache updates remain optional and retain per-agent values", () => {
  const cache: CacheInfo = { session: { hitRate: 0.7, input: 20, output: 4, cacheRead: 14, cacheWrite: 3 }, agents: [{ agentId: "a", generation: 2, model: "m", input: 20, output: 4, cacheRead: 14, cacheWrite: 3, hitRate: 0.7 }] };
  const state = reduce(emptyState(), "cache.updated", cache);
  assert.equal(state.cache?.session.hitRate, 0.7);
  assert.equal(emptyState().cache, undefined);
});

test("cache reducer accepts metrics snapshot usage shape", () => {
  const state = reduce(emptyState(), "cache.updated", {
    session: { hitRate: 0.95, usage: { input: 2, output: 1, cacheRead: 38, cacheWrite: 0 } },
    agents: [{ agentId: "a", generation: 3, model: "sonnet", hitRate: 0.95, usage: { input: 2, output: 1, cacheRead: 38, cacheWrite: 0 } }],
  });
  assert.equal(state.cache?.session.cacheRead, 38);
  assert.equal(state.cache?.agents[0].input, 2);
});

test("agent escalation updates agent from task event", () => {
  let state = reduce(emptyState(), "agent.created", { id: "a", taskId: "task-a", title: "Worker", role: "worker", tier: "tier1", model: "haiku", attempt: 1, status: "running" });
  state = reduce(state, "agent.escalated", { taskId: "task-a", from: "haiku", to: "sonnet", attempt: 2 });
  assert.equal(state.agents[0].escalatedFrom, "haiku");
  assert.equal(state.agents[0].model, "sonnet");
  assert.equal(state.agents[0].attempt, 2);
});

test("backend.status carries routing mode and context limit into state", () => {
  let state = reduce(emptyState(), "backend.status", { ready: true, routingMode: "advanced", contextLimit: 64000, models: [{ id: "m", provider: "p", name: "M", context: 200000, efforts: ["low"] }] });
  assert.deepEqual(state.routing, { mode: "advanced", contextLimit: 64000 });
  assert.equal(state.backend.models[0].context, 200000);
  state = reduce(state, "backend.status", { ready: true });
  assert.deepEqual(state.routing, { mode: "advanced", contextLimit: 64000 });
  state = reduce(state, "backend.status", { routingMode: "bogus" });
  assert.deepEqual(state.routing, { mode: "advanced", contextLimit: 64000 });
});

test("routingFrom rejects malformed input and clamps limit", () => {
  assert.equal(routingFrom(undefined), undefined);
  assert.equal(routingFrom({ mode: "x" }), undefined);
  assert.deepEqual(routingFrom({ mode: "simple" }), { mode: "simple", contextLimit: 0 });
  assert.deepEqual(routingFrom({ mode: "simple", contextLimit: -5 }), { mode: "simple", contextLimit: 0 });
  assert.deepEqual(routingFrom({ mode: "simple", contextLimit: "nan" }), { mode: "simple", contextLimit: 0 });
});

test("protocol names settings.apply and providers.discover", () => {
  const src = readFileSync("src/protocol.ts", "utf8");
  assert.match(src, /"settings\.apply"/);
  assert.match(src, /"providers\.discover"/);
});

test("agent rev guard drops stale, merges equal/newer and legacy partial payloads", () => {
  let s = reduce(emptyState(), "agent.updated", { id: "a", role: "primary", status: "running", activity: "x", rev: 5, phase: "tool", tools: 2 } as never);
  s = reduce(s, "agent.updated", { id: "a", phase: "writing", rev: 4 } as never);
  assert.equal(s.agents[0].phase, "tool", "older rev dropped");
  s = reduce(s, "agent.updated", { id: "a", phase: "writing", rev: 5 } as never);
  assert.equal(s.agents[0].phase, "writing", "equal rev merges");
  s = reduce(s, "agent.updated", { id: "a", tools: 3 } as never);
  assert.equal(s.agents[0].tools, 3, "missing rev merges");
  assert.equal(s.agents[0].rev, 5);
  s = reduce(s, "agent.failed", { id: "a", error: "boom" });
  assert.equal(s.agents[0].status, "running", "legacy partial without status keeps existing status");
  s = reduce(s, "agent.activity", { id: "a", activity: "idle" });
  assert.equal(s.agents[0].activity, "idle");
});

test("full agent payload clears phase with empty string", () => {
  let s = reduce(emptyState(), "agent.updated", { id: "a", status: "running", phase: "tool", phaseAt: 1, rev: 1 } as never);
  s = reduce(s, "agent.completed", { id: "a", status: "completed", phase: "", endedAt: 9, rev: 2 } as never);
  assert.equal(s.agents[0].phase, "");
  assert.equal(s.agents[0].endedAt, 9);
  assert.equal(s.agents[0].phaseAt, 1);
});

test("agent.updated upserts new agent with full fields", () => {
  const s = reduce(emptyState(), "agent.updated", { id: "b", role: "worker", tier: "tier1", title: "W", status: "queued", model: "m", scope: "", workspace: "", generation: 0, budget: 0, usedTokens: 0, activity: "", startedAt: 3, inputTokens: 4, outputTokens: 5, rev: 1 } as never);
  assert.equal(s.agents.length, 1);
  assert.equal(s.agents[0].startedAt, 3);
  assert.equal(s.agents[0].inputTokens, 4);
});

test("chat.delta with cumulative text is idempotent and never shrinks on stale replay", () => {
  let s = reduce(emptyState(), "chat.delta", { agentId: "a", delta: "hel", text: "hel" });
  s = reduce(s, "chat.delta", { agentId: "a", delta: "lo", text: "hello" });
  const once = s.messages.map((m) => ({ ...m }));
  s = reduce(s, "chat.delta", { agentId: "a", delta: "lo", text: "hello" });
  assert.deepEqual(s.messages.map((m) => m.text), once.map((m) => m.text));
  assert.equal(s.messages.length, 1);
  s = reduce(s, "chat.delta", { agentId: "a", delta: "", text: "hel" });
  assert.equal(s.messages[0].text, "hello", "older shorter prefix ignored");
  assert.equal(s.messages[0].id, "stream-a");
});

test("chat.delta with empty text and empty delta creates nothing", () => {
  const s = reduce(emptyState(), "chat.delta", { agentId: "a", delta: "", text: "" });
  assert.equal(s.messages.length, 0);
});

test("chat.discard removes the streaming bubble for that agent only", () => {
  let s = reduce(emptyState(), "chat.delta", { agentId: "a", delta: "x", text: "x" });
  s = reduce(s, "chat.delta", { agentId: "b", delta: "y", text: "y" });
  s = reduce(s, "chat.discard", { agentId: "a" });
  assert.deepEqual(s.messages.map((m) => m.agentId), ["b"]);
  s = reduce(s, "chat.discard", { agentId: "zzz" });
  assert.equal(s.messages.length, 1);
});

test("chat.message dedupes by id and finalizes the stream for its agent", () => {
  let s = reduce(emptyState(), "chat.delta", { agentId: "a", delta: "par", text: "par" });
  s = reduce(s, "chat.message", { id: "m9", role: "assistant", agentId: "a", text: "partial done", createdAt: "t" });
  s = reduce(s, "chat.message", { id: "m9", role: "assistant", agentId: "a", text: "partial done", createdAt: "t" });
  assert.equal(s.messages.length, 1);
  assert.equal(s.messages[0].streaming, false);
  s = reduce(s, "chat.message", { id: "e1", role: "error", text: "oops", createdAt: "t" });
  assert.equal(s.messages.at(-1)?.role, "error");
});

test("terminal agent finalize drops empty stream and keeps non-empty as final", () => {
  let s = reduce(emptyState(), "agent.created", { id: "a", status: "running", rev: 1 } as never);
  s = reduce(s, "chat.delta", { agentId: "a", delta: "", text: "" });
  s = reduce(s, "chat.delta", { agentId: "a", delta: "", text: "" });
  s = reduce(s, "chat.delta", { agentId: "b", delta: "", text: "", });
  s = reduce(s, "chat.delta", { agentId: "a", delta: "partial", text: "partial" });
  s = reduce(s, "agent.failed", { id: "a", status: "failed", rev: 2 } as never);
  assert.equal(s.messages.length, 1);
  assert.equal(s.messages[0].streaming, false);
  assert.equal(s.messages[0].text, "partial");
  s = reduce(s, "chat.delta", { agentId: "c", delta: "", text: "gone" });
  s = reduce(s, "agent.completed", { id: "c", status: "completed", rev: 1 } as never);
  const kept = s.messages.filter((m) => m.agentId === "c");
  assert.equal(kept.length, 1);
  assert.equal(kept[0].streaming, false);
});

test("session.running is carried on session state", () => {
  let s = reduce(emptyState(), "session.updated", { id: "s", objective: "o", mode: "build", status: "active", running: true });
  assert.equal(s.session?.running, true);
  s = reduce(s, "session.updated", { id: "s", objective: "o", mode: "build", status: "active", running: false });
  assert.equal(s.session?.running, false);
});

test("Events list includes agent.updated and chat.discard", () => {
  assert.ok((Events as readonly string[]).includes("agent.updated"));
  assert.ok((Events as readonly string[]).includes("chat.discard"));
});

test("port helper does not throw", () => {
  const srv = createServer();
  srv.listen(0);
  srv.close();
});

test("a new or opened session replaces everything that was on screen", () => {
  let s = emptyState();
  s = reduce(s, "agent.created", { id: "a1", title: "Primary", role: "primary", status: "running" });
  s = reduce(s, "task.created", { id: "t1", title: "Probe", status: "queued" });
  s = reduce(s, "chat.message", { id: "m1", role: "user", text: "hello", createdAt: "now" });
  s = reduce(s, "changes.updated", [{ id: "c1", agentId: "a1", taskId: "t1", path: "a.ts", status: "proposed" }]);
  s = reduce(s, "verification.completed", { id: "v1", command: "make test", status: "failed" });
  for (const event of ["session.created", "session.opened"]) {
    const next = reduce(s, event, { id: "sess_2", objective: "", mode: "build", status: "active" });
    assert.equal(next.session?.id, "sess_2", event);
    assert.deepEqual([next.agents, next.tasks, next.messages, next.changes, next.verification, next.findings], [[], [], [], [], [], []], event);
  }
  // session.updated is the same session changing, so it must not clear anything.
  const same = reduce(s, "session.updated", { id: "sess_1", objective: "x", mode: "build", status: "active", running: true });
  assert.equal(same.messages.length, 1);
  assert.equal(same.agents.length, 1);
});

test("change records carry kind and line counts through the reducer", () => {
  const s = reduce(emptyState(), "changes.updated", [{ id: "c1", agentId: "a", taskId: "t", path: "a.ts", status: "proposed", kind: "added", added: 3, deleted: 0 }]);
  assert.equal(s.changes[0].kind, "added");
  assert.equal(s.changes[0].added, 3);
});
