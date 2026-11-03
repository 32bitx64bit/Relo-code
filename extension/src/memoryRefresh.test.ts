import assert from "node:assert/strict";
import { test } from "node:test";
import { loadMemoryList, MemoryRefresh, MEMORY_REFRESH_DEBOUNCE_MS } from "./memoryRefresh";

test("loadMemoryList asks memory.list and returns its items, or ok:false when the reply carries no list", async () => {
  const calls: unknown[][] = [];
  const good = await loadMemoryList(async (method, params) => { calls.push([method, params]); return { items: [{ id: "mem_a" }] }; });
  assert.deepEqual(calls, [["memory.list", {}]]);
  assert.deepEqual(good, { ok: true, items: [{ id: "mem_a" }] });
  assert.deepEqual(await loadMemoryList(async () => ({})), { ok: false, items: [] });
  assert.deepEqual(await loadMemoryList(async () => null), { ok: false, items: [] });
});

const settle = (ms = MEMORY_REFRESH_DEBOUNCE_MS + 60) => new Promise((r) => setTimeout(r, ms));

test("a burst of triggers loads the list once, debounced", async () => {
  let loads = 0;
  const r = new MemoryRefresh({ enabled: () => true, load: async () => { loads += 1; }, debounceMs: 20 });
  r.trigger();
  r.trigger();
  r.trigger();
  assert.equal(loads, 0, "nothing loads before the debounce elapses");
  assert.equal(r.pending, true);
  await settle(80);
  assert.equal(loads, 1);
  assert.equal(r.triggeredCount, 1);
  r.dispose();
});

test("with relo.context.projectMemory off no load is requested", async () => {
  let enabled = false;
  let loads = 0;
  const r = new MemoryRefresh({ enabled: () => enabled, load: async () => { loads += 1; }, debounceMs: 20 });
  r.trigger();
  assert.equal(r.pending, false);
  await settle(80);
  assert.equal(loads, 0);
  assert.equal(r.triggeredCount, 0);
  enabled = true;
  r.trigger();
  await settle(80);
  assert.equal(loads, 1, "turning the flag on makes the next trigger load again");
  r.dispose();
});

test("a load still running is joined, not started twice, and a failed load does not reject", async () => {
  let resolve: () => void = () => {};
  let loads = 0;
  const r = new MemoryRefresh({
    enabled: () => true,
    load: () => { loads += 1; return new Promise<void>((res) => { resolve = res; }); },
    debounceMs: 10,
  });
  const first = r.run();
  const second = r.run();
  assert.equal(loads, 1, "the second trigger joins the request in flight");
  resolve();
  await Promise.all([first, second]);
  const third = r.run();
  assert.equal(loads, 2, "once settled, the next trigger loads again");
  resolve();
  await third;

  const failing = new MemoryRefresh({ enabled: () => true, load: async () => { throw new Error("core down"); }, debounceMs: 10 });
  await failing.run();
  failing.trigger();
  await settle(80);
  assert.equal(failing.startedCount, 2, "a failing load still counts as started and the next one runs");
  failing.dispose();
  r.dispose();
});

test("a debounced trigger does not fire after dispose", async () => {
  let loads = 0;
  const r = new MemoryRefresh({ enabled: () => true, load: async () => { loads += 1; }, debounceMs: 20 });
  r.trigger();
  r.dispose();
  await settle(80);
  assert.equal(loads, 0);
});
