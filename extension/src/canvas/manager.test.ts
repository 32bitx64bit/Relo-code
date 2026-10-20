import { test } from "node:test";
import * as assert from "node:assert/strict";
import { createRequire } from "node:module";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";

test("Canvas manager implements on-demand editor lifecycle without opening a permanent view", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "relo-canvas-manager-"));
  const commands: Array<[string, ...unknown[]]> = [];
  const documents: unknown[] = [];
  const disposable = () => ({ dispose() {} });
  const uri = (file: string) => ({ fsPath: file, toString: () => file });
  const vscode = {
    workspace: { isTrusted: true, textDocuments: documents, openTextDocument: async (uri: unknown) => ({ uri }), onDidChangeTextDocument: disposable, onDidSaveTextDocument: disposable, createFileSystemWatcher: () => ({ ...disposable(), onDidChange: disposable, onDidDelete: disposable }) },
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
  try {
    assert.equal(commands.length, 0, "activation must not open a Canvas");
    const created = await manager.handle({ sessionId: "session-1", agentId: "agent-1", workspace: root, request: { action: "create", name: "Fantasy RPG Inventory", format: "html", entry: "index.html", files: { "index.html": "<h1>Inventory</h1>" } } }, "session-1") as { artifact: { id: string; revision: number } };
    assert.equal(commands[0][0], "vscode.openWith");
    assert.equal(commands[0][2], "relo.canvas");
    const id = created.artifact.id;
    assert.equal((await manager.store.get(id)).owner, "agent-1", "an artifact records the agent that created it");
    const inspected = await manager.handle({ sessionId: "session-1", agentId: "agent-1", workspace: root, request: { action: "inspect", id } }, "session-1") as { artifact: { revision: number; files: string[] }; source: { files: Record<string, string> } };
    assert.equal(inspected.artifact.revision, 1);
    assert.match(inspected.source.files["index.html"], /Inventory/);
    const updated = await manager.execute({ action: "update", id, revision: 1, files: { "index.html": '<body style="background:#151515">Dark stone inventory</body>' } }) as { artifact: { revision: number } };
    assert.equal(updated.artifact.revision, 2);
    await manager.execute({ action: "close", id });
    await manager.execute({ action: "open", id });
    assert.equal((await manager.store.get(id)).files["index.html"].includes("Dark stone"), true);
    await manager.execute({ action: "popout", id });
    await assert.rejects(manager.handle({ sessionId: "stale", agentId: "agent-1", workspace: root, request: { action: "list" } }, "current"), /different session/);
    vscode.workspace.isTrusted = false;
    await assert.rejects(manager.execute({ action: "capture", id }), /trusted workspace/);
    assert.equal((await manager.execute({ action: "list" }) as { artifacts: unknown[] }).artifacts.length, 1);
  } finally { manager.dispose(); await rm(root, { recursive: true, force: true }); }
});
