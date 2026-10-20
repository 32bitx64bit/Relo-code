export const CANVAS_DOCUMENT_VERSION = 1 as const;
export const CANVAS_MAX_DOCUMENT_BYTES = 4 * 1024 * 1024;
export const CANVAS_MAX_FILES = 64;
export const CANVAS_FILE_EXTENSION = ".relo-canvas";
export const CANVAS_FORMATS = ["html", "react", "svg"] as const;
export const CANVAS_ACTIONS = ["create", "open", "update", "inspect", "capture", "popout", "close", "list"] as const;

export type CanvasFormat = (typeof CANVAS_FORMATS)[number];
export type CanvasAction = (typeof CANVAS_ACTIONS)[number];

export interface CanvasAsset {
  mime: string;
  /** Canonical base64 payload, without a data: URL prefix. */
  data: string;
}

/**
 * Source of truth for one Canvas artifact. Updates replace `files` and `assets`
 * wholesale; they are not merged or patched.
 */
export interface CanvasDocument {
  version: 1;
  id: string;
  name: string;
  format: CanvasFormat;
  entry: string;
  files: Record<string, string>;
  assets?: Record<string, CanvasAsset>;
  /**
   * The agent that owns the artifact, taken from the core envelope that carried the request.
   * It is absent on artifacts written before ownership was recorded: those belong to nobody,
   * so they are served only to a caller the core allowed to read unowned artifacts.
   */
  owner?: string;
  createdAt: string;
  updatedAt: string;
  revision: number;
}

export interface CanvasRequest {
  action: CanvasAction;
  id?: string;
  name?: string;
  format?: CanvasFormat;
  entry?: string;
  files?: Record<string, string>;
  assets?: Record<string, CanvasAsset>;
  revision?: number;
  width?: number;
  height?: number;
}

export interface CanvasDiagnostic {
  code: string;
  message: string;
  file?: string;
  line?: number;
  column?: number;
}

export class CanvasError extends Error {
  readonly code: string;
  readonly diagnostics: CanvasDiagnostic[];

  constructor(code: string, message: string, diagnostics?: CanvasDiagnostic[]) {
    super(message);
    this.name = "CanvasError";
    this.code = code;
    this.diagnostics = diagnostics ?? [{ code, message }];
  }
}

type Obj = Record<string, unknown>;

const CANVAS_ID = /^[a-z0-9][a-z0-9-]{0,79}$/;
const PATH_SEGMENT = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/;
const BASE64 = /^[A-Za-z0-9+/]*={0,2}$/;
const RESERVED_SEGMENTS = new Set(["__proto__", "constructor", "prototype"]);
const SOURCE_EXTENSIONS = new Set(["html", "htm", "css", "js", "mjs", "jsx", "ts", "tsx", "json", "svg", "md", "txt"]);
const ENTRY_EXTENSIONS: Record<CanvasFormat, readonly string[]> = {
  html: ["html", "htm"],
  react: ["jsx", "tsx", "js", "ts"],
  svg: ["svg"],
};
const ASSET_MIME_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp", "font/woff", "font/woff2"]);

function isObject(value: unknown): value is Obj {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isAction(value: unknown): value is CanvasAction {
  return typeof value === "string" && (CANVAS_ACTIONS as readonly string[]).includes(value);
}

function isFormat(value: unknown): value is CanvasFormat {
  return typeof value === "string" && (CANVAS_FORMATS as readonly string[]).includes(value);
}

function fail(code: string, message: string, file?: string): never {
  const diagnostic: CanvasDiagnostic = file === undefined ? { code, message } : { code, message, file };
  throw new CanvasError(code, message, [diagnostic]);
}

function present<T>(value: T | null | undefined): T | undefined {
  return value === null || value === undefined ? undefined : value;
}

/** Returns why a relative path is unsafe, or undefined when it is acceptable. */
function relativePathProblem(value: unknown): string | undefined {
  if (typeof value !== "string" || value.length === 0 || value.length > 255) return "must be a non-empty string of at most 255 characters";
  if (value.includes("\\") || value.includes("\0")) return "must use forward slashes and contain no NUL or backslash";
  if (value.startsWith("/") || /^[A-Za-z]:/.test(value)) return "must be relative";
  for (const segment of value.split("/")) {
    if (segment === "" || segment === "." || segment === "..") return "must not contain empty, '.' or '..' segments";
    if (RESERVED_SEGMENTS.has(segment)) return `uses reserved name ${segment}`;
    if (!PATH_SEGMENT.test(segment)) return `segment ${segment} contains unsupported characters`;
  }
  return undefined;
}

function sourcePath(value: unknown): string {
  const problem = relativePathProblem(value);
  if (problem) fail("invalid_path", `Canvas file path ${String(value)} ${problem}`, String(value));
  const name = value as string;
  const base = name.slice(name.lastIndexOf("/") + 1);
  const dot = base.lastIndexOf(".");
  const extension = dot < 0 ? "" : base.slice(dot + 1).toLowerCase();
  if (!SOURCE_EXTENSIONS.has(extension)) fail("unsupported_file", `Canvas file ${name} has an unsupported extension`, name);
  return name;
}

export function isCanvasId(value: unknown): value is string {
  return typeof value === "string" && CANVAS_ID.test(value);
}

export function assertCanvasId(value: unknown): string {
  if (!isCanvasId(value)) fail("invalid_id", `Invalid Canvas ID: ${JSON.stringify(value)}`);
  return value;
}

function validateName(value: unknown): string {
  if (typeof value !== "string") fail("invalid_document", "Canvas name must be a string");
  const name = value.trim();
  if (name.length === 0 || name.length > 120) fail("invalid_document", "Canvas name must be 1 to 120 characters");
  return name;
}

function validateFormat(value: unknown): CanvasFormat {
  if (!isFormat(value)) fail("invalid_document", `Canvas format must be one of ${CANVAS_FORMATS.join(", ")}`);
  return value;
}

function validateTimestamp(value: unknown, label: string): string {
  if (typeof value !== "string" || Number.isNaN(Date.parse(value))) fail("invalid_document", `${label} must be an ISO timestamp`);
  return value;
}

function validateRevision(value: unknown): number {
  if (!Number.isInteger(value) || (value as number) < 1) fail("invalid_document", "Canvas revision must be an integer of at least 1");
  return value as number;
}

/** The owner is the agent id the core envelope carried. It is optional so documents written before
 * ownership was recorded keep parsing. */
function validateOwner(value: unknown): string | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "string" || value.length === 0 || value.length > 128) {
    fail("invalid_document", "Canvas owner must be a string of 1 to 128 characters");
  }
  return value;
}

function validateFiles(value: unknown): Record<string, string> {
  if (!isObject(value)) fail("invalid_document", "Canvas files must be an object of path to source text");
  const keys = Object.keys(value);
  if (keys.length === 0) fail("invalid_document", "Canvas must contain at least one file");
  if (keys.length > CANVAS_MAX_FILES) fail("too_many_files", `Canvas may contain at most ${CANVAS_MAX_FILES} files`);
  const lower = new Set<string>();
  const files: Record<string, string> = {};
  for (const key of keys) {
    const name = sourcePath(key);
    if (lower.has(name.toLowerCase())) fail("duplicate_path", `Canvas file ${name} collides with another path on case-insensitive filesystems`, name);
    lower.add(name.toLowerCase());
    for (const other of keys) {
      if (other.startsWith(`${name}/`)) fail("path_conflict", `Canvas file ${name} conflicts with directory ${other}`, name);
    }
    const text = value[key];
    if (typeof text !== "string") fail("invalid_document", `Canvas file ${name} must be a string`, name);
    if (Buffer.byteLength(text, "utf8") > 512 * 1024) fail("document_too_large", `Canvas file ${name} exceeds 512 KiB`, name);
    files[name] = text;
  }
  return files;
}

function isCanonicalBase64(value: string): boolean {
  return value.length % 4 === 0 && BASE64.test(value) && Buffer.from(value, "base64").toString("base64") === value;
}

function validateAssets(value: unknown): Record<string, CanvasAsset> | undefined {
  if (value === undefined || value === null) return undefined;
  if (!isObject(value)) fail("invalid_document", "Canvas assets must be an object");
  const keys = Object.keys(value);
  if (keys.length > CANVAS_MAX_FILES) fail("too_many_files", `Canvas may contain at most ${CANVAS_MAX_FILES} assets`);
  const lower = new Set<string>();
  const assets: Record<string, CanvasAsset> = {};
  for (const key of keys) {
    const problem = relativePathProblem(key);
    if (problem) fail("invalid_path", `Canvas asset ${key} ${problem}`, key);
    if (lower.has(key.toLowerCase())) fail("duplicate_path", `Canvas asset ${key} collides with another path`, key);
    lower.add(key.toLowerCase());
    const asset = value[key];
    if (!isObject(asset)) fail("invalid_asset", `Canvas asset ${key} must be an object with mime and data`, key);
    if (typeof asset.mime !== "string" || !ASSET_MIME_TYPES.has(asset.mime)) fail("invalid_asset", `Canvas asset ${key} has an unsupported MIME type`, key);
    if (typeof asset.data !== "string" || !isCanonicalBase64(asset.data)) fail("invalid_asset", `Canvas asset ${key} must contain canonical base64 data`, key);
    if (Buffer.byteLength(asset.data, "base64") > CANVAS_MAX_DOCUMENT_BYTES) fail("document_too_large", `Canvas asset ${key} exceeds the size limit`, key);
    assets[key] = { mime: asset.mime, data: asset.data };
  }
  return assets;
}

function validateEntry(value: unknown, format: CanvasFormat, files: Record<string, string>): string {
  if (typeof value !== "string") fail("invalid_document", "Canvas entry must be a string");
  const entry = sourcePath(value);
  if (!Object.prototype.hasOwnProperty.call(files, entry)) fail("missing_entry", `Canvas entry ${entry} is not one of the files`, entry);
  const extension = entry.slice(entry.lastIndexOf(".") + 1).toLowerCase();
  if (!ENTRY_EXTENSIONS[format].includes(extension)) fail("invalid_entry", `Canvas entry ${entry} does not suit format ${format}`, entry);
  return entry;
}

/** Validates an untrusted value and returns a normalized, size-checked document. */
export function parseCanvasDocument(raw: unknown): CanvasDocument {
  if (!isObject(raw)) fail("invalid_document", "Canvas document must be an object");
  if (raw.version !== CANVAS_DOCUMENT_VERSION) fail("invalid_document", "Canvas document version must be 1");
  const id = assertCanvasId(raw.id);
  const name = validateName(raw.name);
  const format = validateFormat(raw.format);
  const files = validateFiles(raw.files);
  const entry = validateEntry(raw.entry, format, files);
  const assets = validateAssets(raw.assets);
  const owner = validateOwner(raw.owner);
  const document: CanvasDocument = {
    version: CANVAS_DOCUMENT_VERSION,
    id,
    name,
    format,
    entry,
    files,
    ...(assets ? { assets } : {}),
    ...(owner ? { owner } : {}),
    createdAt: validateTimestamp(raw.createdAt, "createdAt"),
    updatedAt: validateTimestamp(raw.updatedAt, "updatedAt"),
    revision: validateRevision(raw.revision),
  };
  if (Buffer.byteLength(JSON.stringify(document), "utf8") > CANVAS_MAX_DOCUMENT_BYTES) {
    fail("document_too_large", `Canvas document exceeds ${CANVAS_MAX_DOCUMENT_BYTES} bytes`);
  }
  return document;
}

/** Validates an agent or editor request. Fields are checked here; relations between them are checked by the store. */
export function parseCanvasRequest(raw: unknown): CanvasRequest {
  if (!isObject(raw)) fail("invalid_request", "Canvas request must be an object");
  const allowed = new Set(["action", "id", "name", "format", "entry", "files", "assets", "revision", "width", "height"]);
  if (Object.keys(raw).some((key) => !allowed.has(key))) fail("invalid_request", "Unknown Canvas request field");
  if (Buffer.byteLength(JSON.stringify(raw), "utf8") > CANVAS_MAX_DOCUMENT_BYTES) fail("document_too_large", "Canvas request exceeds 4 MiB");
  if (!isAction(raw.action)) fail("invalid_request", `Unsupported Canvas action: ${String(raw.action)}`);
  const request: CanvasRequest = { action: raw.action };
  const id = present(raw.id);
  if (id !== undefined) request.id = assertCanvasId(id);
  const name = present(raw.name);
  if (name !== undefined) request.name = validateName(name);
  const format = present(raw.format);
  if (format !== undefined) request.format = validateFormat(format);
  const entry = present(raw.entry);
  if (entry !== undefined) request.entry = sourcePath(entry);
  const files = present(raw.files);
  if (files !== undefined) request.files = validateFiles(files);
  const assets = validateAssets(raw.assets);
  if (assets !== undefined) request.assets = assets;
  const revision = present(raw.revision);
  if (revision !== undefined) request.revision = validateRevision(revision);
  for (const key of ["width", "height"] as const) {
    const size = present(raw[key]);
    if (size === undefined) continue;
    if (!Number.isInteger(size) || (size as number) < 1 || (size as number) > 10000) fail("invalid_request", `${key} must be an integer from 1 to 10000`);
    request[key] = size as number;
  }
  if (request.action === "create") {
    for (const key of ["name", "format", "entry", "files"] as const) {
      if (request[key] === undefined) fail("invalid_request", `create requires ${key}`);
    }
  }
  if (request.action === "update") {
    if (request.id === undefined) fail("invalid_request", "update requires id");
    if (request.revision === undefined) fail("revision_required", "update requires the revision it was read at");
  }
  return request;
}
