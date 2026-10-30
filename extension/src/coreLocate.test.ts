import assert from "node:assert/strict";
import { test } from "node:test";
import * as path from "node:path";
import { locateCore } from "./coreLocate";

const ext = path.join("/home/u/.vscode-oss/extensions", "relocode.relo-code-0.1.0");
const unrelatedWs = "/home/u/work/unknown-healths";
const reloWs = "/home/u/relo";

function existing(...paths: string[]) {
  const set = new Set(paths);
  return (p: string) => set.has(p);
}

test("installed bundled core is found when the workspace is not the Relo repo", () => {
  const bundled = path.join(ext, "bin", "relo-core-linux-amd64");
  const got = locateCore(ext, unrelatedWs, "linux", "x64", existing(bundled));
  assert.deepEqual(got, { cmd: bundled });
});

test("bundled core wins over a workspace core checkout", () => {
  const bundled = path.join(ext, "bin", "relo-core-linux-amd64");
  const wsCore = path.join(reloWs, "core", "cmd", "relo-core");
  const got = locateCore(ext, reloWs, "linux", "x64", existing(bundled, wsCore));
  assert.deepEqual(got, { cmd: bundled });
});

test("workspace core is used only when nothing is bundled", () => {
  const wsCore = path.join(reloWs, "core", "cmd", "relo-core");
  const got = locateCore(ext, reloWs, "linux", "x64", existing(wsCore));
  assert.deepEqual(got, { cmd: "go", cwd: path.join(reloWs, "core") });
});

test("missing bundled core in an unrelated workspace fails with a reinstall hint", () => {
  assert.throws(
    () => locateCore(ext, unrelatedWs, "linux", "x64", existing()),
    (err: Error) => err.message.includes("relo-core-linux-amd64 not found") && err.message.includes("make install"),
  );
});

test("windows binary name carries .exe and arm64 maps to arm64", () => {
  const bundled = path.join(ext, "bin", "relo-core-windows-amd64.exe");
  assert.deepEqual(locateCore(ext, unrelatedWs, "win32", "x64", existing(bundled)), { cmd: bundled });
  const arm = path.join(ext, "bin", "relo-core-darwin-arm64");
  assert.deepEqual(locateCore(ext, unrelatedWs, "darwin", "arm64", existing(arm)), { cmd: arm });
});
