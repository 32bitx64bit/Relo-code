// Pure helpers for the webview -> extension bridge. No vscode import so they run under tsx --test.

export type SettingSchema = {
  type: string;
  enum?: string[];
  description?: string;
  minimum?: number;
  maximum?: number;
  default?: unknown;
};

export type SettingsSchema = Record<string, SettingSchema>;

export const RELO_PREFIX = "relo.";
export const MAX_ARRAY_LENGTH = 50;
export const MAX_ARRAY_JSON = 200000;

function jsonLength(value: unknown): number {
  try {
    return JSON.stringify(value).length;
  } catch {
    return Number.POSITIVE_INFINITY;
  }
}

// Commands the webview may trigger. Anything else is dropped.
export const COMMAND_ALLOWLIST = new Set<string>([
  "relo.newSession",
  "relo.sessionHistory",
  "relo.dashboard",
  "relo.pause",
  "relo.resume",
  "relo.stopAll",
  "relo.refreshPrimary",
  "relo.restartBackend",
  "relo.openLogs",
  "relo.showCacheStats",
  "relo.selectPrimaryModel",
  "relo.agent.stop",
  "relo.agent.refresh",
  "relo.agent.inspect",
  "relo.agent.openWorktree",
  "relo.changes.accept",
  "relo.changes.reject",
  "relo.changes.diff",
  "relo.changes.acceptAll",
  "relo.changes.rejectAll",
]);

// Commands that take a single {id} payload.
export const ID_COMMANDS = new Set<string>([
  "relo.agent.stop",
  "relo.agent.refresh",
  "relo.agent.inspect",
  "relo.agent.openWorktree",
  "relo.changes.accept",
  "relo.changes.reject",
  "relo.changes.diff",
]);

const SESSION_ID_RE = /^[A-Za-z0-9_.-]{1,200}$/;

// A session id from the webview: the ids core makes (sess_<hex>) pass, anything else is dropped.
export function checkSessionId(id: unknown): string | undefined {
  return typeof id === "string" && SESSION_ID_RE.test(id) ? id : undefined;
}

export function isAllowedCommand(id: unknown): id is string {
  return typeof id === "string" && COMMAND_ALLOWLIST.has(id);
}

export type CommandCheck = { ok: true; command: string; args: unknown[] } | { ok: false; reason: string };

// Validates a webview `command` message. ID commands require a non-empty string id.
export function checkCommand(msg: { command?: unknown; id?: unknown }): CommandCheck {
  if (!isAllowedCommand(msg.command)) return { ok: false, reason: "command not allowed" };
  if (ID_COMMANDS.has(msg.command)) {
    if (typeof msg.id !== "string" || !msg.id || msg.id.length > 200) return { ok: false, reason: "id required" };
    return { ok: true, command: msg.command, args: [{ id: msg.id }] };
  }
  return { ok: true, command: msg.command, args: [] };
}

export type SettingCheck = { ok: true; subkey: string; value: unknown } | { ok: false; reason: string };

// Validates a settings.set message against the package.json schema. Only "relo.<subkey>" keys in the schema pass.
export function checkSetting(key: unknown, value: unknown, schema: SettingsSchema): SettingCheck {
  if (typeof key !== "string" || !key.startsWith(RELO_PREFIX)) return { ok: false, reason: "key must start with relo." };
  const entry = Object.prototype.hasOwnProperty.call(schema, key) ? schema[key] : undefined;
  if (!entry) return { ok: false, reason: "unknown setting" };
  const subkey = key.slice(RELO_PREFIX.length);
  switch (entry.type) {
    case "string":
      if (typeof value !== "string") return { ok: false, reason: "expected string" };
      if (entry.enum && !entry.enum.includes(value)) return { ok: false, reason: "value not in enum" };
      if (value.length > 4000) return { ok: false, reason: "string too long" };
      break;
    case "boolean":
      if (typeof value !== "boolean") return { ok: false, reason: "expected boolean" };
      break;
    case "number":
      if (typeof value !== "number" || !Number.isFinite(value)) return { ok: false, reason: "expected finite number" };
      if (entry.minimum !== undefined && value < entry.minimum) return { ok: false, reason: "below minimum" };
      if (entry.maximum !== undefined && value > entry.maximum) return { ok: false, reason: "above maximum" };
      break;
    case "object":
      if (!value || typeof value !== "object" || Array.isArray(value)) return { ok: false, reason: "expected object" };
      break;
    case "array": {
      if (!Array.isArray(value)) return { ok: false, reason: "expected array" };
      if (value.length > MAX_ARRAY_LENGTH) return { ok: false, reason: "array too long" };
      if (jsonLength(value) >= MAX_ARRAY_JSON) return { ok: false, reason: "array too large" };
      break;
    }
    default:
      return { ok: false, reason: "unsupported type" };
  }
  return { ok: true, subkey, value };
}

// Builds the settings reply: every relo.* key with its current value, default, and schema.
export function settingsReply(
  values: Record<string, unknown>,
  schema: SettingsSchema,
  models: unknown[],
): { type: "settings"; values: Record<string, unknown>; defaults: Record<string, unknown>; schema: SettingsSchema; models: unknown[] } {
  const defaults: Record<string, unknown> = {};
  const outSchema: SettingsSchema = {};
  for (const key of Object.keys(schema)) {
    if (!key.startsWith(RELO_PREFIX)) continue;
    outSchema[key] = schema[key];
    defaults[key] = schema[key].default;
  }
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(outSchema)) out[key] = values[key] ?? defaults[key];
  return { type: "settings", values: out, defaults, schema: outSchema, models };
}

// What the webview's model pickers would show for a catalog: provider, id, limits and effort levels. Two catalogs
// with the same signature need no new settings message.
export function modelsSignature(models: unknown): string {
  if (!Array.isArray(models)) return "";
  return models
    .map((m) => {
      const x = (m ?? {}) as { provider?: unknown; id?: unknown; context?: unknown; output?: unknown; efforts?: unknown };
      const efforts = Array.isArray(x.efforts) ? x.efforts.join(",") : "";
      return `${String(x.provider)}/${String(x.id)}:${String(x.context ?? 0)}:${String(x.output ?? 0)}:${efforts}`;
    })
    .join("\n");
}
