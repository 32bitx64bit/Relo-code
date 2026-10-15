import assert from "node:assert/strict";
import { test } from "node:test";
import { CrashBreaker } from "./crashBreaker";

test("allows isolated crashes", () => {
  let t = 0;
  const b = new CrashBreaker(3, 1000, () => t);
  assert.equal(b.record(), true);
  t = 2000;
  assert.equal(b.record(), true);
  t = 4000;
  assert.equal(b.record(), true);
  assert.equal(b.tripped, false);
});

test("trips on the third crash inside the window and stays tripped", () => {
  let t = 0;
  const b = new CrashBreaker(3, 1000, () => t);
  assert.equal(b.record(), true);
  t = 100;
  assert.equal(b.record(), true);
  t = 200;
  assert.equal(b.record(), false);
  assert.equal(b.tripped, true);
  t = 9000;
  assert.equal(b.record(), false);
});

test("reset forgets earlier crashes", () => {
  let t = 0;
  const b = new CrashBreaker(2, 1000, () => t);
  b.record();
  t = 10;
  assert.equal(b.record(), false);
  b.reset();
  assert.equal(b.tripped, false);
  t = 20;
  assert.equal(b.record(), true);
});
