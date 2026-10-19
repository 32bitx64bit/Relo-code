import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { after, before, describe, it } from "node:test";
import { CanvasStore } from "./store";
import { CanvasError, parseCanvasDocument, parseCanvasRequest, CANVAS_MAX_FILES } from "./types";

let root: string;
let outside: string;

before(async () => {
  root = await fs.mkdtemp(path.join(os.tmpdir(), "relo-canvas-"));
  outside = await fs.mkdtemp(path.join(os.tmpdir(), "relo-canvas-outside-"));
});

after(async () => {
  await fs.rm(root, { recursive: true, force: true });
  await fs.rm(outside, { recursive: true, force: true });
});

function createRequest(overrides: Record<string, unknown> = {}) {
  return {
    action: "create",
    name: "Dashboard Demo",
    format: "html",
    entry: "index.html",
    files: { "index.html": "<h1>Hello</h1>" },
    ...overrides,
  };
}

async function freshRoot(): Promise<string> {
  return fs.mkdtemp(path.join(root, "case-"));
}

describe("CanvasStore persistence", () => {
  it("creates an artifact at revision 1 with stable timestamps and a slug-plus-suffix ID", async () => {
    const store = new CanvasStore(() => root);
    const doc = await store.create(createRequest({ name: "My Chart!!" }));
    assert.equal(doc.version, 1);
    assert.equal(doc.revision, 1);
    assert.equal(doc.createdAt, doc.updatedAt);
    assert.match(doc.id, /^my-chart-[0-9a-f]{8}$/);
    assert.equal(store.artifactPath(doc.id), path.join(root, ".relo", "canvas", `${doc.id}.relo-canvas`));
    const onDisk = JSON.parse(await fs.readFile(store.artifactPath(doc.id), "utf8"));
    assert.deepEqual(onDisk, doc);
  });

  it("generates distinct IDs for identical names", async () => {
    const store = new CanvasStore(() => root);
    const a = await store.create(createRequest());
    const b = await store.create(createRequest());
    assert.notEqual(a.id, b.id);
  });

  it("survives reopening the store from a new instance", async () => {
    const dir = await freshRoot();
    const first = new CanvasStore(() => dir);
    const created = await first.create(createRequest());
    const reopened = new CanvasStore(() => dir);
    const loaded = await reopened.get(created.id);
    assert.deepEqual(loaded, created);
    const listed = await reopened.list();
    assert.deepEqual(listed.map((item) => item.id), [created.id]);
  });

  it("returns an empty list when the canvas directory does not exist", async () => {
    const store = new CanvasStore(() => path.join(root, "never-used"));
    await fs.mkdir(path.join(root, "never-used"), { recursive: true });
    assert.deepEqual(await store.list(), []);
  });

  it("lists only valid artifacts and skips corrupt files", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const good = await store.create(createRequest());
    await fs.writeFile(path.join(dir, ".relo", "canvas", "broken.relo-canvas"), "{not json");
    await fs.writeFile(path.join(dir, ".relo", "canvas", "notes.txt"), "ignored");
    const listed = await store.list();
    assert.deepEqual(listed.map((item) => item.id), [good.id]);
  });

  it("writes atomically and leaves no temp files behind", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    await store.create(createRequest());
    const entries = await fs.readdir(path.join(dir, ".relo", "canvas"));
    assert.equal(entries.filter((name) => name.endsWith(".tmp")).length, 0);
  });
});

describe("CanvasStore updates", () => {
  it("applies an update at the matching revision and bumps revision while keeping createdAt", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest());
    const updated = await store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "<h1>v2</h1>" } });
    assert.equal(updated.revision, 2);
    assert.equal(updated.createdAt, created.createdAt);
    assert.equal(updated.files["index.html"], "<h1>v2</h1>");
    assert.equal((await store.get(created.id)).revision, 2);
  });

  it("replaces files wholesale rather than merging them", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest({ files: { "index.html": "a", "app.js": "b" } }));
    const updated = await store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "a2" } });
    assert.deepEqual(Object.keys(updated.files), ["index.html"]);
  });

  it("rejects a stale revision with revision_conflict and leaves the file unchanged", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest());
    await store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "winner" } });
    await assert.rejects(
      store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "loser" } }),
      (error: unknown) => error instanceof CanvasError && error.code === "revision_conflict",
    );
    assert.equal((await store.get(created.id)).files["index.html"], "winner");
  });

  it("requires a revision on update", () => {
    assert.throws(
      () => parseCanvasRequest({ action: "update", id: "abc", files: { "index.html": "x" } }),
      (error: unknown) => error instanceof CanvasError && error.code === "revision_required",
    );
  });

  it("serializes concurrent updates so exactly one of two same-revision writers succeeds", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest());
    const results = await Promise.allSettled([
      store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "A" } }),
      store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "B" } }),
    ]);
    assert.equal(results.filter((result) => result.status === "fulfilled").length, 1);
    const rejected = results.find((result): result is PromiseRejectedResult => result.status === "rejected");
    assert.ok(rejected, "one writer must be rejected");
    assert.equal((rejected.reason as CanvasError).code, "revision_conflict");
    assert.equal((await store.get(created.id)).revision, 2);
  });

  it("returns not_found for an unknown ID", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    await assert.rejects(store.get("missing-00000000"), (error: unknown) => error instanceof CanvasError && error.code === "not_found");
  });
});

describe("path and symlink safety", () => {
  it("rejects traversal, absolute, and prototype file names in create", async () => {
    const store = new CanvasStore(() => root);
    for (const name of ["../escape.html", "/etc/passwd", "__proto__", "a/../b.html", "C:/x.html", "dir\\x.html"]) {
      await assert.rejects(
        store.create(createRequest({ files: { [name]: "x" }, entry: name })),
        (error: unknown) => error instanceof CanvasError && error.code === "invalid_path",
        name,
      );
    }
  });

  it("rejects an entry that is not among the files", async () => {
    const store = new CanvasStore(() => root);
    await assert.rejects(store.create(createRequest({ entry: "missing.html" })), (error: unknown) => error instanceof CanvasError && error.code === "missing_entry");
  });

  it("refuses to follow a symlinked .relo directory that points outside the repository", async () => {
    const dir = await freshRoot();
    await fs.symlink(outside, path.join(dir, ".relo"));
    const store = new CanvasStore(() => dir);
    await assert.rejects(store.create(createRequest()), (error: unknown) => error instanceof CanvasError && error.code === "symlink_escape");
    assert.deepEqual(await fs.readdir(outside), []);
  });

  it("refuses to read an artifact that is a symlink to a file outside the repository", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest());
    const target = path.join(outside, "secret.relo-canvas");
    await fs.writeFile(target, await fs.readFile(store.artifactPath(created.id), "utf8"));
    await fs.rm(store.artifactPath(created.id));
    await fs.symlink(target, store.artifactPath(created.id));
    await assert.rejects(store.get(created.id), (error: unknown) => error instanceof CanvasError && error.code === "symlink_escape");
    await assert.rejects(store.update({ action: "update", id: created.id, revision: 1, files: { "index.html": "x" } }), (error: unknown) => error instanceof CanvasError && error.code === "symlink_escape");
  });

  it("rejects an artifact whose embedded ID does not match its filename", async () => {
    const dir = await freshRoot();
    const store = new CanvasStore(() => dir);
    const created = await store.create(createRequest());
    const other = parseCanvasDocument({ ...(await store.get(created.id)), id: "other-00000000" });
    await fs.writeFile(path.join(dir, ".relo", "canvas", "spoof-00000000.relo-canvas"), JSON.stringify(other));
    await assert.rejects(store.get("spoof-00000000"), (error: unknown) => error instanceof CanvasError && error.code === "invalid_document");
  });

  it("rejects invalid IDs before touching the filesystem", () => {
    const store = new CanvasStore(() => root);
    assert.throws(() => store.artifactPath("../etc/passwd"), (error: unknown) => error instanceof CanvasError && error.code === "invalid_id");
  });
});

describe("document validation", () => {
  const valid = {
    version: 1,
    id: "demo-abcd1234",
    name: "Demo",
    format: "html",
    entry: "index.html",
    files: { "index.html": "<p>hi</p>" },
    createdAt: "2026-10-19T00:00:00.000Z",
    updatedAt: "2026-10-19T00:00:00.000Z",
    revision: 1,
  };

  it("accepts a valid document", () => {
    assert.equal(parseCanvasDocument(valid).id, "demo-abcd1234");
  });

  it("rejects an unknown format", () => {
    assert.throws(() => parseCanvasDocument({ ...valid, format: "vue" }), (error: unknown) => error instanceof CanvasError && error.code === "invalid_document");
  });

  it("rejects a wrong version", () => {
    assert.throws(() => parseCanvasDocument({ ...valid, version: 2 }), (error: unknown) => error instanceof CanvasError && error.code === "invalid_document");
  });

  it("rejects more than 64 files", () => {
    const files: Record<string, string> = {};
    for (let i = 0; i <= CANVAS_MAX_FILES; i++) files[`f${i}.js`] = "";
    assert.throws(() => parseCanvasDocument({ ...valid, files, entry: "f0.js", format: "react" }), (error: unknown) => error instanceof CanvasError && error.code === "too_many_files");
  });

  it("rejects a document larger than 4 MiB", () => {
    const big = "x".repeat(4 * 1024 * 1024 + 1);
    assert.throws(() => parseCanvasDocument({ ...valid, files: { "index.html": big } }), (error: unknown) => error instanceof CanvasError && error.code === "document_too_large");
  });

  it("rejects a non-base64 or wrong-MIME asset", () => {
    const withAsset = (asset: unknown) => ({ ...valid, assets: { "logo.png": asset } });
    assert.throws(() => parseCanvasDocument(withAsset({ mime: "image/png", data: "not base64!!" })), (error: unknown) => error instanceof CanvasError && error.code === "invalid_asset");
    assert.throws(() => parseCanvasDocument(withAsset({ mime: "text/html", data: "aGk=" })), (error: unknown) => error instanceof CanvasError && error.code === "invalid_asset");
  });

  it("accepts a canonical base64 asset", () => {
    const doc = parseCanvasDocument({ ...valid, assets: { "logo.png": { mime: "image/png", data: "aGk=" } } });
    assert.deepEqual(doc.assets, { "logo.png": { mime: "image/png", data: "aGk=" } });
  });

  it("rejects an unsupported file extension", () => {
    assert.throws(() => parseCanvasDocument({ ...valid, files: { "index.exe": "x" }, entry: "index.exe" }), (error: unknown) => error instanceof CanvasError && error.code === "unsupported_file");
  });

  it("rejects an invalid timestamp and revision", () => {
    assert.throws(() => parseCanvasDocument({ ...valid, createdAt: "yesterday" }), (error: unknown) => error instanceof CanvasError);
    assert.throws(() => parseCanvasDocument({ ...valid, revision: 0 }), (error: unknown) => error instanceof CanvasError);
  });

  it("rejects an unknown action in a request", () => {
    assert.throws(() => parseCanvasRequest({ action: "delete" }), (error: unknown) => error instanceof CanvasError && error.code === "invalid_request");
  });
});
