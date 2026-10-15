import assert from "node:assert/strict";
import { test } from "node:test";
import { applyCollapse, cleanCollapsed, MAX_COLLAPSED } from "./uiState";

test("cleanCollapsed keeps only valid keys set to true", () => {
  const raw = JSON.parse('{"g:models":true,"p:acme":true,"p:x":false,"g:y":"yes","bad":true,"p:":true,"__proto__":true,"p:a b":true}');
  assert.deepEqual(cleanCollapsed(raw), { "g:models": true, "p:acme": true });
});

test("cleanCollapsed rejects non-objects", () => {
  for (const v of [null, undefined, 1, "x", [], ["g:a"]]) assert.deepEqual(cleanCollapsed(v), {});
});

test("cleanCollapsed caps the number of keys", () => {
  const raw: Record<string, true> = {};
  for (let i = 0; i < MAX_COLLAPSED + 50; i++) raw["p:m" + i] = true;
  assert.equal(Object.keys(cleanCollapsed(raw)).length, MAX_COLLAPSED);
});

test("applyCollapse sets, clears, and ignores bad input", () => {
  let s = applyCollapse({}, "p:acme", true);
  assert.deepEqual(s, { "p:acme": true });
  s = applyCollapse(s, "g:models", true);
  assert.deepEqual(s, { "p:acme": true, "g:models": true });
  s = applyCollapse(s, "p:acme", false);
  assert.deepEqual(s, { "g:models": true });
  assert.equal(applyCollapse(s, "p:acme", "no"), s);
  assert.equal(applyCollapse(s, "../../etc", true), s);
  assert.equal(applyCollapse(s, 5, true), s);
  assert.equal(applyCollapse(s, "constructor", true), s);
});

test("applyCollapse refuses to grow past the cap", () => {
  let s = {} as Record<string, true>;
  for (let i = 0; i < MAX_COLLAPSED; i++) s = applyCollapse(s, "p:m" + i, true);
  assert.equal(Object.keys(s).length, MAX_COLLAPSED);
  assert.equal(applyCollapse(s, "p:extra", true), s);
});
