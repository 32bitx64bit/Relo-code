import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { after, before, describe, it } from "node:test";
import { createRequire } from "node:module";
import { CanvasStore } from "./store";
import { CanvasError, parseCanvasDocument } from "./types";

let root: string;

before(async () => {
  root = await fs.mkdtemp(path.join(os.tmpdir(), "relo-canvas-owner-"));
});

after(async () => {
  await fs.rm(root, { recursive: true, force: true });
});

function createRequest(overrides: Record<string, unknown> = {}) {
  return {
    action: "create",
    name: "Owned Artifact",
    format: "html",
    entry: "index.html",
    files: { "index.html": "<h1>Owned</h1>" },
    ...overrides,
  };
}

async function freshRoot(): Promise<string> {
  return fs.mkdtemp(path.join(root, "case-"));
}

/** A document written before ownership was recorded: same layout, no owner field. */
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

describe("CanvasStore ownership", () => {
  it("records the owner agent id on the artifact and on disk", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest(), "agent-a");
    assert.equal(created.owner, "agent-a");
    const onDisk = JSON.parse(await fs.readFile(store.artifactPath(created.id), "utf8"));
    assert.equal(onDisk.owner, "agent-a");
    assert.equal(onDisk.version, 1, "the document version must not change");
    assert.equal(store.artifactPath(created.id), path.join(dir, ".relo", "canvas", `${created.id}.relo-canvas`));
  });

  it("lists only the artifacts of the requesting agent", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const mine = await store.create(createRequest({ name: "Mine" }), "agent-a");
    const theirs = await store.create(createRequest({ name: "Theirs" }), "agent-b");
    assert.deepEqual((await store.list("agent-a")).map((item) => item.id), [mine.id]);
    assert.deepEqual((await store.list("agent-b")).map((item) => item.id), [theirs.id]);
    assert.deepEqual((await store.list("agent-c")).map((item) => item.id), []);
  });

  it("refuses to read an artifact owned by another agent", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest(), "agent-a");
    await assert.rejects(
      store.get(created.id, "agent-b"),
      (error: unknown) => error instanceof CanvasError && error.code === "not_owned",
    );
    assert.equal((await store.get(created.id, "agent-a")).id, created.id);
  });

  it("refuses to update an artifact owned by another agent", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest(), "agent-a");
    await assert.rejects(
      store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "stolen" } }, "agent-b"),
      (error: unknown) => error instanceof CanvasError && error.code === "not_owned",
    );
    assert.equal((await store.get(created.id, "agent-a")).files["index.html"], "<h1>Owned</h1>");
  });

  it("hides an artifact written before ownership was recorded from a caller that may not read one", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    await store.create(createRequest({ name: "Seed" }), "agent-a");
    await fs.writeFile(path.join(dir, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));
    assert.deepEqual((await store.list("agent-b")).map((item) => item.id), [], "an unowned artifact is not listed for a caller that may not read one");
    await assert.rejects(
      store.get("legacy-00000000", "agent-b"),
      (error: unknown) => error instanceof CanvasError && error.code === "not_owned",
    );
    assert.equal(parseCanvasDocument(legacyDocument).owner, undefined);
  });

  it("keeps an artifact written before ownership was recorded readable for a caller that may read one", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    await store.create(createRequest({ name: "Seed" }), "agent-a");
    await fs.writeFile(path.join(dir, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));
    const listed = await store.list("agent-b", true);
    assert.deepEqual(listed.map((item) => item.id).sort(), ["legacy-00000000"]);
    assert.equal((await store.get("legacy-00000000", "agent-b", true)).files["index.html"], "<p>legacy</p>");
  });

  it("keeps the owner when its owner updates the artifact", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest(), "agent-a");
    const updated = await store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "<h1>Edited</h1>" } }, "agent-a");
    assert.equal(updated.owner, "agent-a");
    assert.equal((await store.get(created.id, "agent-a")).owner, "agent-a");
  });

  it("claims an unowned artifact when an agent that may read one writes to it", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    await store.create(createRequest({ name: "Seed" }), "agent-a");
    await fs.writeFile(path.join(dir, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));
    await assert.rejects(
      store.update({ action: "update", id: "legacy-00000000", revision: 1, files: { "index.html": "<p>claimed</p>" } }, "agent-b"),
      (error: unknown) => error instanceof CanvasError && error.code === "not_owned",
    );
    const updated = await store.update({ action: "update", id: "legacy-00000000", revision: 1, files: { "index.html": "<p>claimed</p>" } }, "agent-b", true);
    assert.equal(updated.owner, "agent-b");
    await assert.rejects(
      store.get("legacy-00000000", "agent-c"),
      (error: unknown) => error instanceof CanvasError && error.code === "not_owned",
    );
  });

  it("refuses an owner field in the agent's request body", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    await assert.rejects(
      store.create(createRequest({ owner: "agent-b" }) as unknown, "agent-a"),
      (error: unknown) => error instanceof CanvasError && error.code === "invalid_request",
    );
  });

  it("does not filter when no owner is given", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest(), "agent-a");
    assert.deepEqual((await store.list()).map((item) => item.id), [created.id]);
    assert.equal((await store.get(created.id)).id, created.id);
  });

  it("keeps an unowned artifact readable on the editor path, which carries no owner", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    await fs.mkdir(path.join(dir, ".relo", "canvas"), { recursive: true });
    await fs.writeFile(path.join(dir, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));
    assert.deepEqual((await store.list()).map((item) => item.id), ["legacy-00000000"]);
    assert.equal((await store.get("legacy-00000000")).files["index.html"], "<p>legacy</p>");
  });
});

/** Builds a CanvasManager against a stubbed vscode module. */
async function newManager(workspace: string): Promise<{
  manager: import("./manager").CanvasManager;
  dispose: () => void;
}> {
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
  const manager = new Manager(() => workspace);
  return { manager, dispose: () => manager.dispose() };
}

describe("CanvasManager ownership", () => {
  it("scopes list to the agent that made the request", async () => {
    const dir = await freshRoot();
    const { manager, dispose } = await newManager(dir);
    try {
      const created = await manager.handle({
        sessionId: "session-1",
        agentId: "agent-a",
        workspace: dir,
        request: { action: "create", name: "Owned Artifact", format: "html", entry: "index.html", files: { "index.html": "<h1>Owned</h1>" } },
      }, "session-1") as { artifact: { id: string } };
      const mine = await manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: dir, request: { action: "list" } }, "session-1") as { artifacts: Array<{ id: string }> };
      assert.deepEqual(mine.artifacts.map((item) => item.id), [created.artifact.id]);
      const theirs = await manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: dir, request: { action: "list" } }, "session-1") as { artifacts: Array<{ id: string }> };
      assert.deepEqual(theirs.artifacts, []);
    } finally { dispose(); }
  });

  it("refuses to inspect or capture an artifact owned by another agent", async () => {
    const dir = await freshRoot();
    const { manager, dispose } = await newManager(dir);
    try {
      const created = await manager.handle({
        sessionId: "session-1",
        agentId: "agent-a",
        workspace: dir,
        request: { action: "create", name: "Owned Artifact", format: "html", entry: "index.html", files: { "index.html": "<h1>Owned</h1>" } },
      }, "session-1") as { artifact: { id: string } };
      const id = created.artifact.id;
      await assert.rejects(
        manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: dir, request: { action: "inspect", id } }, "session-1"),
        /belongs to another agent/,
      );
      await assert.rejects(
        manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: dir, request: { action: "capture", id } }, "session-1"),
        /belongs to another agent/,
      );
      await assert.rejects(
        manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: dir, request: { action: "open", id } }, "session-1"),
        /belongs to another agent/,
      );
      const inspected = await manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: dir, request: { action: "inspect", id } }, "session-1") as { source: { files: Record<string, string> } };
      assert.match(inspected.source.files["index.html"], /Owned/);
    } finally { dispose(); }
  });

  it("refuses a canvas request that names no agent", async () => {
    const dir = await freshRoot();
    const { manager, dispose } = await newManager(dir);
    try {
      await assert.rejects(
        manager.handle({ sessionId: "session-1", workspace: dir, request: { action: "list" } }, "session-1"),
        /does not identify the agent/,
      );
    } finally { dispose(); }
  });

  it("hides a legacy artifact from an agent that may not read an unowned one", async () => {
    const dir = await freshRoot();
    const { manager, dispose } = await newManager(dir);
    try {
      await manager.handle({
        sessionId: "session-1",
        agentId: "agent-a",
        workspace: dir,
        request: { action: "create", name: "Owned Artifact", format: "html", entry: "index.html", files: { "index.html": "<h1>Owned</h1>" } },
      }, "session-1");
      await fs.writeFile(path.join(dir, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));
      const listed = await manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: dir, request: { action: "list" } }, "session-1") as { artifacts: Array<{ id: string; owner?: string }> };
      assert.deepEqual(listed.artifacts, [], "a legacy artifact is not listed for an agent that may not read one");
      await assert.rejects(
        manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: dir, request: { action: "inspect", id: "legacy-00000000" } }, "session-1"),
        (error: unknown) => error instanceof CanvasError && error.code === "not_owned",
      );
      await assert.rejects(
        manager.handle({ sessionId: "session-1", agentId: "agent-b", workspace: dir, request: { action: "open", id: "legacy-00000000" } }, "session-1"),
        (error: unknown) => error instanceof CanvasError && error.code === "not_owned",
      );
    } finally { dispose(); }
  });

  it("keeps a legacy artifact listed and readable for an agent that may read an unowned one", async () => {
    const dir = await freshRoot();
    const { manager, dispose } = await newManager(dir);
    try {
      await manager.handle({
        sessionId: "session-1",
        agentId: "agent-a",
        workspace: dir,
        request: { action: "create", name: "Owned Artifact", format: "html", entry: "index.html", files: { "index.html": "<h1>Owned</h1>" } },
      }, "session-1");
      await fs.writeFile(path.join(dir, ".relo", "canvas", "legacy-00000000.relo-canvas"), JSON.stringify(legacyDocument));
      const listed = await manager.handle({ sessionId: "session-1", agentId: "agent-b", readUnowned: true, workspace: dir, request: { action: "list" } }, "session-1") as { artifacts: Array<{ id: string; owner?: string }> };
      assert.deepEqual(listed.artifacts.map((item) => item.id), ["legacy-00000000"], "a legacy artifact stays listed for an agent that may read one");
      assert.equal(listed.artifacts[0].owner, undefined, "a legacy artifact reports no owner");
      const inspected = await manager.handle({ sessionId: "session-1", agentId: "agent-b", readUnowned: true, workspace: dir, request: { action: "inspect", id: "legacy-00000000" } }, "session-1") as { source: { files: Record<string, string> } };
      assert.match(inspected.source.files["index.html"], /legacy/, "a legacy artifact stays openable for an agent that may read one");
    } finally { dispose(); }
  });

  it("reports the owner of an artifact and keeps every list field", async () => {
    const dir = await freshRoot();
    const { manager, dispose } = await newManager(dir);
    try {
      await manager.handle({
        sessionId: "session-1",
        agentId: "agent-a",
        workspace: dir,
        request: { action: "create", name: "Owned Artifact", format: "html", entry: "index.html", files: { "index.html": "<h1>Owned</h1>" } },
      }, "session-1");
      const listed = await manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: dir, request: { action: "list" } }, "session-1") as { artifacts: Array<Record<string, unknown>> };
      assert.equal(listed.artifacts.length, 1);
      const entry = listed.artifacts[0];
      for (const field of ["id", "name", "format", "entry", "revision", "createdAt", "updatedAt", "files", "assets"]) {
        assert.ok(field in entry, `the list response keeps ${field}`);
      }
      assert.equal(entry.owner, "agent-a", "the list response names the owning agent");
      const inspected = await manager.handle({ sessionId: "session-1", agentId: "agent-a", workspace: dir, request: { action: "inspect", id: entry.id as string } }, "session-1") as { artifact: { owner?: string } };
      assert.equal(inspected.artifact.owner, "agent-a", "the artifact response names the owning agent");
    } finally { dispose(); }
  });
});
