import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { repositoryInfo, snapRepository } from "./repository";

test("repository info follows checked-out branch, nested folders and detached HEAD", async (t) => {
  const root = mkdtempSync(join(tmpdir(), "relo-repository-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const git = (...args: string[]) => execFileSync("git", ["-C", root, ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  git("init", "-b", "feature/current");
  git("config", "user.email", "relo@test");
  git("config", "user.name", "Relo");
  writeFileSync(join(root, "a.txt"), "base\n");
  git("add", "a.txt");
  git("commit", "-m", "base");
  const nested = join(root, "nested");
  mkdirSync(nested);
  assert.deepEqual(await repositoryInfo(nested), { root: nested, gitRoot: root, branch: "feature/current" });
  git("checkout", "-b", "another-branch");
  assert.equal((await repositoryInfo(root)).branch, "another-branch");
  git("checkout", "--detach");
  assert.match((await repositoryInfo(root)).branch, /^detached HEAD \([a-f0-9]+\)$/);
});

test("snapRepository keeps empty setting on the default folder, snaps nested folders to the Git root and rejects non-Git folders", async (t) => {
  const root = mkdtempSync(join(tmpdir(), "relo-snap-"));
  const plain = mkdtempSync(join(tmpdir(), "relo-snap-plain-"));
  t.after(() => {
    rmSync(root, { recursive: true, force: true });
    rmSync(plain, { recursive: true, force: true });
  });
  execFileSync("git", ["init", "-q", root]);
  const nested = join(root, "a", "b");
  mkdirSync(nested, { recursive: true });
  assert.equal(await snapRepository("", plain), plain);
  assert.equal(await snapRepository(nested, plain), root);
  assert.equal(await snapRepository(plain, root), undefined);
});

test("repository info labels non-Git folders without throwing", async (t) => {
  const root = mkdtempSync(join(tmpdir(), "relo-no-repository-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  assert.deepEqual(await repositoryInfo(root), { root, branch: "Not a Git repository" });
});
