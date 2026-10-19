import { randomBytes } from "node:crypto";
import { promises as fs } from "node:fs";
import * as path from "node:path";
import {
  CANVAS_DOCUMENT_VERSION,
  CANVAS_MAX_DOCUMENT_BYTES,
  CANVAS_FILE_EXTENSION,
  CanvasError,
  assertCanvasId,
  isCanvasId,
  parseCanvasDocument,
  parseCanvasRequest,
  type CanvasDocument,
  type CanvasRequest,
} from "./types";

const CANVAS_DIR = path.join(".relo", "canvas");

function errorCode(error: unknown): string | undefined {
  return typeof error === "object" && error !== null && "code" in error ? String((error as { code: unknown }).code) : undefined;
}

function isInside(parent: string, child: string): boolean {
  const relative = path.relative(parent, child);
  return relative === "" || (!relative.startsWith("..") && !path.isAbsolute(relative));
}

function slugify(name: string): string {
  const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 40).replace(/-+$/g, "");
  return slug || "canvas";
}

/**
 * Persists Canvas artifacts as one JSON document per ID under <root>/.relo/canvas.
 * All reads and writes are serialized through a single queue, and writes go through
 * a temp file plus rename so a reader never observes a partial document.
 */
export class CanvasStore {
  private queue: Promise<unknown> = Promise.resolve();

  constructor(private readonly rootGetter: () => string) {}

  /** Absolute path where the artifact with this ID is stored. Pure: performs no filesystem access. */
  artifactPath(id: string): string {
    assertCanvasId(id);
    return path.join(this.rootGetter(), CANVAS_DIR, `${id}${CANVAS_FILE_EXTENSION}`);
  }

  async create(raw: CanvasRequest | unknown): Promise<CanvasDocument> {
    const request = parseCanvasRequest(raw);
    if (request.action !== "create") throw new CanvasError("invalid_request", "create expects action create");
    return this.serial(async () => {
      const dir = await this.ensureDirectory();
      const now = new Date().toISOString();
      const id = request.id ?? await this.uniqueID(dir, request.name ?? "canvas");
      const document = parseCanvasDocument({
        version: CANVAS_DOCUMENT_VERSION,
        id,
        name: request.name,
        format: request.format,
        entry: request.entry,
        files: request.files,
        assets: request.assets,
        createdAt: now,
        updatedAt: now,
        revision: 1,
      });
      await this.write(dir, document, { exclusive: true });
      return document;
    });
  }

  async get(id: string): Promise<CanvasDocument> {
    assertCanvasId(id);
    return this.serial(() => this.read(id));
  }

  /**
   * Replaces name, format, entry, files, and assets when supplied; omitted fields keep their stored value.
   * `files` and `assets` are replaced wholesale, not merged. `revision` must equal the stored revision.
   */
  async update(raw: CanvasRequest | unknown): Promise<CanvasDocument> {
    const request = parseCanvasRequest(raw);
    if (request.action !== "update" || request.id === undefined || request.revision === undefined) {
      throw new CanvasError("invalid_request", "update requires id and revision");
    }
    const id = request.id;
    const expected = request.revision;
    return this.serial(async () => {
      const dir = await this.ensureDirectory();
      const current = await this.read(id);
      if (current.revision !== expected) {
        throw new CanvasError("revision_conflict", `Canvas ${id} is at revision ${current.revision}, not ${expected}; read it again before updating`);
      }
      const document = parseCanvasDocument({
        ...current,
        name: request.name ?? current.name,
        format: request.format ?? current.format,
        entry: request.entry ?? current.entry,
        files: request.files ?? current.files,
        assets: request.assets ?? current.assets,
        createdAt: current.createdAt,
        updatedAt: new Date().toISOString(),
        revision: current.revision + 1,
      });
      await this.write(dir, document, { exclusive: false });
      return document;
    });
  }

  /** Lists valid artifacts sorted by name then ID. Invalid or foreign files are skipped. */
  async list(): Promise<CanvasDocument[]> {
    return this.serial(async () => {
      const dir = await this.existingDirectory();
      if (!dir) return [];
      const entries = await fs.readdir(dir, { withFileTypes: true });
      const documents: CanvasDocument[] = [];
      for (const entry of entries) {
        if (!entry.isFile() || !entry.name.endsWith(CANVAS_FILE_EXTENSION)) continue;
        const id = entry.name.slice(0, -CANVAS_FILE_EXTENSION.length);
        if (!isCanvasId(id)) continue;
        try {
          documents.push(await this.read(id));
        } catch {
          // Corrupt or symlinked artifacts are reported by get(); list stays usable.
        }
      }
      return documents.sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
    });
  }

  private serial<T>(task: () => Promise<T>): Promise<T> {
    const run = this.queue.then(task, task);
    this.queue = run.catch(() => undefined);
    return run;
  }

  /** Creates <root>/.relo/canvas, rejecting symlinked components and any realpath outside the root. */
  private async ensureDirectory(): Promise<string> {
    const root = await this.realRoot();
    let current = root;
    for (const segment of CANVAS_DIR.split(path.sep)) {
      current = path.join(current, segment);
      const stat = await fs.lstat(current).catch((error: unknown) => {
        if (errorCode(error) === "ENOENT") return undefined;
        throw error;
      });
      if (!stat) {
        await fs.mkdir(current);
      } else if (stat.isSymbolicLink()) {
        throw new CanvasError("symlink_escape", `Canvas storage path ${path.relative(root, current)} must not be a symlink`);
      } else if (!stat.isDirectory()) {
        throw new CanvasError("invalid_storage", `Canvas storage path ${path.relative(root, current)} is not a directory`);
      }
    }
    const real = await fs.realpath(current);
    if (!isInside(root, real)) throw new CanvasError("symlink_escape", "Canvas storage directory resolves outside the repository");
    return current;
  }

  private async existingDirectory(): Promise<string | undefined> {
    const root = await this.realRoot();
    const parent = await fs.lstat(path.join(root, ".relo")).catch((error: unknown) => {
      if (errorCode(error) === "ENOENT") return undefined;
      throw error;
    });
    if (!parent) return undefined;
    if (parent.isSymbolicLink() || !parent.isDirectory()) throw new CanvasError("symlink_escape", "Canvas storage parent must be a real directory");
    const dir = path.join(root, CANVAS_DIR);
    const stat = await fs.lstat(dir).catch((error: unknown) => {
      if (errorCode(error) === "ENOENT") return undefined;
      throw error;
    });
    if (!stat) return undefined;
    if (stat.isSymbolicLink() || !stat.isDirectory()) throw new CanvasError("symlink_escape", "Canvas storage directory must be a real directory");
    const real = await fs.realpath(dir);
    if (!isInside(root, real)) throw new CanvasError("symlink_escape", "Canvas storage directory resolves outside the repository");
    return dir;
  }

  private async realRoot(): Promise<string> {
    const root = this.rootGetter();
    if (!path.isAbsolute(root)) throw new CanvasError("invalid_root", "Canvas root must be an absolute path");
    return fs.realpath(root);
  }

  private async read(id: string): Promise<CanvasDocument> {
    const dir = await this.existingDirectory();
    const file = dir ? path.join(dir, `${id}${CANVAS_FILE_EXTENSION}`) : undefined;
    if (!file) throw new CanvasError("not_found", `Canvas ${id} does not exist`);
    const stat = await fs.lstat(file).catch((error: unknown) => {
      if (errorCode(error) === "ENOENT") throw new CanvasError("not_found", `Canvas ${id} does not exist`);
      throw error;
    });
    if (stat.isSymbolicLink()) throw new CanvasError("symlink_escape", `Canvas ${id} must not be a symlink`);
    if (!stat.isFile()) throw new CanvasError("invalid_document", `Canvas ${id} is not a regular file`);
    if (stat.size > CANVAS_MAX_DOCUMENT_BYTES) throw new CanvasError("document_too_large", `Canvas ${id} exceeds 4 MiB`);
    const real = await fs.realpath(file);
    if (!isInside(await this.realRoot(), real)) throw new CanvasError("symlink_escape", `Canvas ${id} resolves outside the repository`);
    let parsed: unknown;
    try {
      parsed = JSON.parse(await fs.readFile(file, "utf8"));
    } catch {
      throw new CanvasError("invalid_document", `Canvas ${id} is not valid JSON`);
    }
    const document = parseCanvasDocument(parsed);
    if (document.id !== id) throw new CanvasError("invalid_document", `Canvas file ${id} contains ID ${document.id}`);
    return document;
  }

  private async uniqueID(dir: string, name: string): Promise<string> {
    const base = slugify(name);
    for (let attempt = 0; attempt < 20; attempt++) {
      const id = `${base}-${randomBytes(4).toString("hex")}`;
      const exists = await fs.lstat(path.join(dir, `${id}${CANVAS_FILE_EXTENSION}`)).then(() => true, (error: unknown) => {
        if (errorCode(error) === "ENOENT") return false;
        throw error;
      });
      if (!exists) return id;
    }
    throw new CanvasError("id_collision", "Could not allocate a unique Canvas ID");
  }

  private async write(dir: string, document: CanvasDocument, options: { exclusive: boolean }): Promise<void> {
    const target = path.join(dir, `${document.id}${CANVAS_FILE_EXTENSION}`);
    const temp = path.join(dir, `.${document.id}.${randomBytes(6).toString("hex")}.tmp`);
    const body = `${JSON.stringify(document)}\n`;
    const handle = await fs.open(temp, "wx", 0o600);
    try {
      await handle.writeFile(body, "utf8");
      await handle.sync();
    } catch (error) {
      await handle.close().catch(() => undefined);
      await fs.rm(temp, { force: true });
      throw error;
    }
    await handle.close();
    try {
      if (options.exclusive) {
        // link fails with EEXIST instead of silently overwriting an existing artifact.
        await fs.link(temp, target);
        await fs.rm(temp, { force: true });
      } else {
        await fs.rename(temp, target);
      }
    } catch (error) {
      await fs.rm(temp, { force: true });
      if (errorCode(error) === "EEXIST") throw new CanvasError("id_collision", `Canvas ${document.id} already exists`);
      throw error;
    }
  }
}
