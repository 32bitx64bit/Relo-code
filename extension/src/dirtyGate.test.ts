import assert from "node:assert/strict";
import { test } from "node:test";
import { dirtyPathsFor } from "./dirtyGate";

test("dirty documents gate only the proposed paths", () => {
  const docs = [
    { path: "src/a.ts", isDirty: true },
    { path: "src/b.ts", isDirty: true },
    { path: "src/c.ts", isDirty: false },
  ];
  assert.deepEqual(dirtyPathsFor(["src/a.ts", "src/c.ts"], docs), ["src/a.ts"]);
});

test("separators and a leading ./ do not hide a dirty file", () => {
  const docs = [{ path: ".\\src\\a.ts", isDirty: true }];
  assert.deepEqual(dirtyPathsFor(["./src/a.ts"], docs), ["src/a.ts"]);
});

test("no dirty document among the target paths means the gate passes", () => {
  assert.deepEqual(dirtyPathsFor(["src/a.ts"], [{ path: "other.ts", isDirty: true }]), []);
});
