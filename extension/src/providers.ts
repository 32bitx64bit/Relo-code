// Pure custom-provider helpers (CONTRACT section 2). No vscode import so they run under tsx --test.
// Only known fields survive sanitization; API keys never pass through here.

export const PROVIDER_ID_RE = /^[a-z0-9][a-z0-9_-]{0,39}$/;
export const MODEL_ID_RE = /^[^\s"'`\\\x00-\x1f\x7f]{1,200}$/;
const EFFORT_RE = /^[a-z0-9][a-z0-9_-]{0,19}$/;
const MAX_TOKENS = 10_000_000;
const MAX_BASE_URL = 500;
export const MAX_PROVIDERS = 50;
export const MAX_MODELS = 200;
export const MAX_EFFORTS = 10;
export const MAX_DISCOVERED = 1000;

// Built-in OpenCode provider ids. A custom provider must not shadow them.
export const RESERVED_PROVIDER_IDS: ReadonlySet<string> = new Set([
  "anthropic", "openai", "google", "openrouter", "opencode", "azure", "amazon-bedrock", "groq", "mistral", "xai", "deepseek", "github-copilot",
]);

export type ProviderKind = "openai" | "anthropic";

export interface ProviderModel {
  id: string;
  name: string;
  context?: number;
  output?: number;
  reasoning?: boolean;
  efforts?: string[];
  /** Whether the model accepts images; absent when nobody said, so the core uses the catalog or a guess from the id. */
  vision?: boolean;
  /** The API this model is spoken to in when it differs from its provider's: Claude behind an OpenAI-compatible router. */
  kind?: ProviderKind;
}

export interface ProviderConfig {
  id: string;
  name: string;
  kind: ProviderKind;
  baseURL: string;
  models: ProviderModel[];
}

export type ProviderCheck = { ok: true; provider: ProviderConfig } | { ok: false; reason: string };
export type ModelCheck = { ok: true; model: ProviderModel } | { ok: false; reason: string };
export type BaseURLCheck = { ok: true; url: string } | { ok: false; reason: string };

// Secret storage key for a provider API key. The key value itself is never stored in settings.
export function secretKey(id: string): string {
  return `relo.providerKey.${id}`;
}

// Env var name core uses for a provider key: RELO_KEY_<ID upper, non-alnum -> _>.
export function envKeyName(id: string): string {
  return `RELO_KEY_${id.toUpperCase().replace(/[^A-Z0-9]/g, "_")}`;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

function tokenField(v: unknown): number | undefined {
  if (typeof v !== "number" || !Number.isSafeInteger(v) || v < 0 || v > MAX_TOKENS) return undefined;
  return v;
}

export function validateBaseURL(v: unknown): BaseURLCheck {
  if (typeof v !== "string") return { ok: false, reason: "baseURL must be a string" };
  const url = v.trim();
  if (!url || url.length > MAX_BASE_URL) return { ok: false, reason: `baseURL must be 1..${MAX_BASE_URL} chars` };
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return { ok: false, reason: "baseURL is not a valid URL" };
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return { ok: false, reason: "baseURL must be http or https" };
  if (parsed.username || parsed.password) return { ok: false, reason: "baseURL must not contain credentials" };
  return { ok: true, url };
}

export function validateModel(m: unknown): ModelCheck {
  if (!isRecord(m)) return { ok: false, reason: "model must be an object" };
  const id = m.id;
  if (typeof id !== "string" || !MODEL_ID_RE.test(id)) return { ok: false, reason: "invalid model id (no spaces or quotes, max 200)" };
  const rawName = m.name ?? id;
  if (typeof rawName !== "string" || rawName.length > 200) return { ok: false, reason: "invalid model name (max 200 chars)" };
  const model: ProviderModel = { id, name: rawName.trim() || id };
  if (m.context != null) {
    const n = tokenField(m.context);
    if (n === undefined) return { ok: false, reason: "model context must be an integer 0..10000000" };
    model.context = n;
  }
  if (m.output != null) {
    const n = tokenField(m.output);
    if (n === undefined) return { ok: false, reason: "model output must be an integer 0..10000000" };
    model.output = n;
  }
  if (m.reasoning != null) {
    if (typeof m.reasoning !== "boolean") return { ok: false, reason: "model reasoning must be a boolean" };
    model.reasoning = m.reasoning;
  }
  if (m.efforts != null) {
    if (!Array.isArray(m.efforts) || m.efforts.length > MAX_EFFORTS) return { ok: false, reason: `model efforts must be an array of at most ${MAX_EFFORTS} strings` };
    const efforts: string[] = [];
    for (const e of m.efforts) {
      if (typeof e !== "string" || !EFFORT_RE.test(e)) return { ok: false, reason: "invalid model effort name" };
      efforts.push(e);
    }
    model.efforts = efforts;
  }
  if (m.vision != null) {
    if (typeof m.vision !== "boolean") return { ok: false, reason: "model vision must be a boolean" };
    model.vision = m.vision;
  }
  if (m.kind != null) {
    if (m.kind !== "openai" && m.kind !== "anthropic") return { ok: false, reason: "model kind must be openai or anthropic" };
    model.kind = m.kind;
  }
  return { ok: true, model };
}

// Validates one provider object and returns a fresh object with only known fields.
export function validateProvider(p: unknown): ProviderCheck {
  if (!isRecord(p)) return { ok: false, reason: "provider must be an object" };
  const id = p.id;
  if (typeof id !== "string" || !PROVIDER_ID_RE.test(id)) return { ok: false, reason: "invalid id (lowercase letters, digits, - or _; max 40)" };
  if (RESERVED_PROVIDER_IDS.has(id)) return { ok: false, reason: "id is reserved for a built-in provider" };
  const kind: ProviderKind | undefined = p.kind === "openai" ? "openai" : p.kind === "anthropic" ? "anthropic" : undefined;
  if (!kind) return { ok: false, reason: "kind must be openai or anthropic" };
  const base = validateBaseURL(p.baseURL);
  if (!base.ok) return base;
  const rawName = p.name ?? id;
  if (typeof rawName !== "string" || rawName.length > 100) return { ok: false, reason: "invalid name (max 100 chars)" };
  const rawModels = p.models ?? [];
  if (!Array.isArray(rawModels) || rawModels.length > MAX_MODELS) return { ok: false, reason: `models must be an array of at most ${MAX_MODELS}` };
  const models: ProviderModel[] = [];
  const seen = new Set<string>();
  for (const m of rawModels) {
    const r = validateModel(m);
    if (!r.ok) return r;
    if (seen.has(r.model.id)) return { ok: false, reason: "duplicate model id" };
    seen.add(r.model.id);
    models.push(r.model);
  }
  return { ok: true, provider: { id, name: rawName.trim() || id, kind, baseURL: base.url, models } };
}

// Validates a whole providers list. Invalid, duplicate, or env-colliding entries are dropped and reported in errors.
export function sanitizeProviderList(list: unknown): { providers: ProviderConfig[]; errors: string[] } {
  const providers: ProviderConfig[] = [];
  const errors: string[] = [];
  if (list === undefined || list === null) return { providers, errors };
  if (!Array.isArray(list)) return { providers, errors: ["providers must be an array"] };
  if (list.length > MAX_PROVIDERS) errors.push(`at most ${MAX_PROVIDERS} providers; extra entries dropped`);
  const ids = new Set<string>();
  const envNames = new Set<string>();
  for (const item of list.slice(0, MAX_PROVIDERS)) {
    const r = validateProvider(item);
    if (!r.ok) {
      errors.push(r.reason);
      continue;
    }
    const { id } = r.provider;
    if (ids.has(id)) {
      errors.push(`duplicate provider id ${id}`);
      continue;
    }
    const env = envKeyName(id);
    if (envNames.has(env)) {
      errors.push(`provider id ${id} maps to the same key env var as another provider`);
      continue;
    }
    ids.add(id);
    envNames.add(env);
    providers.push(r.provider);
  }
  return { providers, errors };
}

// Sanitizes models returned by providers.discover. Invalid entries are dropped.
export function sanitizeDiscovered(list: unknown): ProviderModel[] {
  if (!Array.isArray(list)) return [];
  const out: ProviderModel[] = [];
  const seen = new Set<string>();
  for (const m of list.slice(0, MAX_DISCOVERED)) {
    const r = validateModel(m);
    if (!r.ok || seen.has(r.model.id)) continue;
    seen.add(r.model.id);
    out.push(r.model);
  }
  return out;
}
