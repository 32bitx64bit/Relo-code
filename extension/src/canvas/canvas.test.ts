import { test } from "node:test";
import * as assert from "node:assert/strict";
import { createRequire } from "node:module";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import type { CanvasManager } from "./manager";

const legacyDocument = {
  version: 1,
  id: "legacy-00000000",
  name: "Legacy Artifact",
  format: "html",
  entry: "index.html",
  files: { "index.html": "<p>legacy</p>" },
  createdAt: "2026-10-19T00:00:00.000Z",
  updatedAt: "2026-10-19T00:00:00.000Z",
  revision: 1,
};

function loadManager(root: string): { manager: CanvasManager; dispose: () => void } {
  const commands: Array<[string, ...unknown[]]> = [];
  const documents: unknown[] = [];
  const disposable = () => ({ dispose() {} });
  const uri = (file: string) => ({ fsPath: file, toString: () => file });
  const vscode = {
    workspace: { isTrusted: true, textDocuments: documents, openTextDocument: async (target: unknown) => ({ uri: target }), onDidChangeTextDocument: disposable, onDidSaveTextDocument: disposable, createFileSystemWatcher: () => ({ ...disposable(), onDidChange: disposable, onDidDelete: disposable }) },
    window: { registerCustomEditorProvider: disposable, showQuickPick: async () => undefined },
    commands: { executeCommand: async (command: string, ...args: unknown[]) => { commands.push([command, ...args]); } },
    env: { openExternal: async () => true },
    Uri: { file: uri, parse: uri }, ViewColumn: { Active: 1 }, RelativePattern: class {},
  };
  const require = createRequire(__filename);
  const module = require("node:module") as { _load: (name: string, ...rest: unknown[]) => unknown };
  const load = module._load;
  module._load = (name, ...rest) => name === "vscode" ? vscode : load(name, ...rest);
  let Manager: typeof import("./manager").CanvasManager;
  try { Manager = (require("./manager") as typeof import("./manager")).CanvasManager; } finally { module._load = load; }
  const manager = new Manager(() => root);
  return { manager, dispose: () => manager.dispose() };
}

async function createFor(manager: CanvasManager, root: string, agentId: string, name: string): Promise<string> {
  const created = await manager.handle({
    sessionId: "session-1",
    agentId,
    workspace: root,
    request: { action: "create", name, format: "html", entry: "index.html", files: { "index.html": `<h1>${name}</h1>` } },
  }, "session-1") as { artifact: { id: string } };
  return created.artifact.id;
}

test("owner-scoped list returns the requesting agent's artifacts, and a legacy one only to a caller that may read it", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "relo-canvas-scope-"));
  const { manager, dispose } = loadManager(root);
  try {
    const mine = await createFor(manager, root, "agent-a", "Mine");
    const theirs = await createFor(manager, root, "agent-b", "Theirs");
    await mkdir(path.join(root, ".relo", "canvas"), { recursive: true });
    await writeFile(path.join(root, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));

    const listedA = await manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: root, request: { action: "list" } }, "session-1") as { artifacts: Array<{ id: string }> };
    assert.deepEqual(listedA.artifacts.map((item) => item.id), [mine], "a legacy artifact is hidden from a caller that may not read one");
    assert.ok(!listedA.artifacts.some((item) => item.id === theirs), "agent-a must not list agent-b's artifact");

    const listedB = await manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: root, request: { action: "list" } }, "session-1") as { artifacts: Array<{ id: string }> };
    assert.deepEqual(listedB.artifacts.map((item) => item.id), [theirs], "a legacy artifact is hidden from a caller that may not read one");

    const listedLegacy = await manager.handle({ sessionId: "session-1", agentId: "agent-b", readUnowned: true, workspace: root, request: { action: "list" } }, "session-1") as { artifacts: Array<{ id: string }> };
    assert.deepEqual(listedLegacy.artifacts.map((item) => item.id).sort(), [theirs, "legacy-00000000"].sort(), "a caller that may read an unowned artifact sees it");
  } finally { dispose(); await rm(root, { recursive: true, force: true }); }
});

test("open is refused for another agent's artifact, and for a legacy one the caller may not read", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "relo-canvas-open-"));
  const { manager, dispose } = loadManager(root);
  try {
    const mine = await createFor(manager, root, "agent-a", "Mine");
    const theirs = await createFor(manager, root, "agent-b", "Theirs");
    await mkdir(path.join(root, ".relo", "canvas"), { recursive: true });
    await writeFile(path.join(root, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));

    await assert.rejects(
      manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: root, request: { action: "open", id: theirs } }, "session-1"),
      /belongs to another agent/,
    );
    await assert.rejects(
      manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: root, request: { action: "open", id: "legacy-00000000" } }, "session-1"),
      /has no owner/,
    );
    await manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: root, request: { action: "open", id: mine } }, "session-1");
    await manager.handle({ sessionId: "session-1", agentId: "agent-b", readUnowned: true, workspace: root, request: { action: "open", id: "legacy-00000000" } }, "session-1");
  } finally { dispose(); await rm(root, { recursive: true, force: true }); }
});

test("inspect and capture refuse another agent's artifact before any browser is started", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "relo-canvas-capture-"));
  const { manager, dispose } = loadManager(root);
  try {
    const theirs = await createFor(manager, root, "agent-b", "Theirs");
    await assert.rejects(
      manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: root, request: { action: "inspect", id: theirs } }, "session-1"),
      /belongs to another agent/,
    );
    await assert.rejects(
      manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: root, request: { action: "capture", id: theirs } }, "session-1"),
      /belongs to another agent/,
    );
  } finally { dispose(); await rm(root, { recursive: true, force: true }); }
});
