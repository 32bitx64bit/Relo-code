export const PROTOCOL_VERSION = 1;

export const Methods = {
  initialize: "initialize", shutdown: "shutdown",
  sessionCreate: "session.create", sessionGet: "session.get", sessionList: "session.list", sessionClear: "session.clear", sessionOpen: "session.open", sessionDelete: "session.delete",
  promptSubmit: "prompt.submit", promptCancel: "prompt.cancel", promptRetry: "prompt.retry",
  agentList: "agent.list", agentGet: "agent.get", agentStop: "agent.stop", agentRedirect: "agent.redirect", agentMessage: "agent.message", agentChangeModel: "agent.changeModel", agentRefreshContext: "agent.refreshContext", agentInspectContext: "agent.inspectContext", agentOpenWorktree: "agent.openWorktree",
  taskList: "task.list", taskGet: "task.get", taskCancel: "task.cancel", taskRetry: "task.retry", taskSetPriority: "task.setPriority",
  contextGet: "context.get", contextGenerations: "context.generations", contextRefresh: "context.refresh", contextRebuild: "context.rebuild", contextPin: "context.pin", contextRemove: "context.remove", contextPromote: "context.promote",
  findingsList: "findings.list", memoryList: "memory.list", memoryUpdate: "memory.update", memoryDelete: "memory.delete",
  changesList: "changes.list", changesAccept: "changes.accept", changesReject: "changes.reject", changesAcceptAll: "changes.acceptAll", changesRejectAll: "changes.rejectAll", changesDiff: "changes.diff", changesUndo: "changes.undo", changesRebase: "changes.rebase", changesResolve: "changes.resolve", changesResolveConflicts: "changes.resolveConflicts", workspaceDirty: "workspace.dirty", verificationList: "verification.list",
  modelsList: "models.list", modelsSet: "models.set", backendStatus: "backend.status", backendRestart: "backend.restart", settingsApply: "settings.apply", providersDiscover: "providers.discover", modeSet: "mode.set",
  orchestratorPause: "orchestrator.pause", orchestratorResume: "orchestrator.resume", orchestratorStopAll: "orchestrator.stopAll",
  permissionRespond: "permission.respond", logsTail: "logs.tail", snapshot: "snapshot", cacheGet: "cache.get",
} as const;

export type Method = (typeof Methods)[keyof typeof Methods];

export type FrameKind = "req" | "res" | "err" | "evt";

export interface RpcError {
  code: string;
  message: string;
  data?: unknown;
}

export interface Frame {
  v: number;
  kind: FrameKind;
  id?: string;
  method?: string;
  params?: unknown;
  result?: unknown;
  error?: RpcError;
}

export type AgentStatus =
  | "queued" | "starting" | "running" | "waiting" | "blocked"
  | "completed" | "failed" | "cancelled" | "review_required" | "interrupted";

export interface AgentInfo {
  id: string;
  parentId?: string;
  role: string;
  tier: string;
  title: string;
  taskId?: string;
  difficulty?: string;
  attempt?: number;
  escalatedFrom?: string;
  status: AgentStatus;
  model: string;
  scope: string;
  workspace: string;
  worktree?: string;
  generation: number;
  budget: number;
  usedTokens: number; // tokens spent so far, summed over every model call
  contextTokens?: number; // what the latest model call held: how full the window is now
  window?: number; // context window planned for the model; 0 or missing when unknown
  activity: string;
  files?: string[];
  // Phase is a short machine label: context|model|reasoning|writing|tool|verifying|approval|waiting|compacting|retry, "" when idle/finished.
  phase?: string;
  phaseAt?: number; // unix ms of last phase change
  startedAt?: number; // unix ms of current run start
  endedAt?: number; // unix ms when terminal; 0/undefined while running
  tools?: number;
  // Monotonic per-agent revision; agent.* updates with a lower rev than stored are dropped.
  rev?: number;
  inputTokens?: number;
  outputTokens?: number;
}

export interface TaskInfo {
  id: string;
  title: string;
  description: string;
  acceptance: string[];
  status: string;
  difficulty?: string;
  attempts?: number;
  model?: string;
  agentId?: string;
  dependencies?: string[];
  attempt: number;
}

export interface ChatMessage {
  id: string;
  role: string;
  text: string;
  agentId?: string;
  createdAt: string;
  streaming?: boolean;
  // Queued marks a user message waiting for the running head agent's next step; delivery re-emits without it.
  queued?: boolean;
}

export interface CacheAgentInfo {
  agentId: string;
  generation: number;
  model: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  hitRate: number;
}

export interface CacheUsage {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

export interface CacheInfo {
  session: { hitRate: number; input: number; output: number; cacheRead: number; cacheWrite: number };
  agents: CacheAgentInfo[];
}

export interface AgentEscalatedPayload extends Partial<AgentInfo> {
  id?: string;
  agentId?: string;
  taskId?: string;
  from?: string;
  to?: string;
  attempt?: number;
  escalatedFrom?: string;
  model?: string;
  reason?: string;
}

export interface ChangeInfo {
  id: string;
  agentId: string;
  taskId: string;
  path: string;
  // proposed: waiting in an agent's worktree; accepted/rejected: decided; applied: edited in place (no Git).
  status: string;
  kind?: "added" | "modified" | "deleted";
  added?: number;
  deleted?: number;
  worktree?: string;
  baseRev?: string;
  needs?: string[];
  lineage?: { id: string; patchHash: string }[];
  landed?: string;
  conflict?: string;
  resolving?: boolean;
  superseded?: boolean;
  supersededBy?: string;
  integration?: {
    state: "clean" | "conflicted" | "blocked" | "unknown";
    kind?: string;
    reason?: string;
    ours?: string;
    since?: string[];
    checkedAt?: number;
    fingerprint?: string;
    hunks?: { oursStart: number; oursLen: number; theirsStart: number; theirsLen: number; conflicting: boolean }[];
  };
}

// One row of the session history list (core: session.list).
export interface SessionSummary {
  id: string;
  title: string;
  mode: string;
  createdAt: string;
  updatedAt: string;
  messages: number;
  pendingChanges: number;
  current: boolean;
}

// Both sides of one change for the editor's diff view (core: changes.diff).
export interface ChangeDiff {
  id: string;
  path: string;
  kind?: string;
  status: string;
  before: string;
  after: string;
  hasBefore: boolean;
  hasAfter: boolean;
  binary: boolean;
  truncated: boolean;
  note?: string;
}

export interface ModelInfo {
  id: string;
  provider: string;
  name: string;
  context?: number;
  output?: number;
  efforts?: string[];
}

export interface RoutingState {
  mode: "simple" | "advanced";
  contextLimit: number;
}

export interface ReloState {
  backend: {
    core: string;
    opencode: string;
    version?: string;
    error?: string;
    ready: boolean;
    models: ModelInfo[];
    primary?: string;
    tier1?: string;
    tier2?: string;
    routingMode?: RoutingState["mode"];
    contextLimit?: number;
  };
  routing?: RoutingState;
  session?: { id: string; objective: string; mode: string; status: string; running?: boolean };
  sessions?: SessionSummary[];
  agents: AgentInfo[];
  tasks: TaskInfo[];
  messages: ChatMessage[];
  changes: ChangeInfo[];
  findings: unknown[];
  memory: unknown[];
  verification: unknown[];
  logs: string[];
  inspection?: unknown;
  cache?: CacheInfo;
  efficiency?: { preset: string; profiles?: Record<string, string> };
}

export interface InitializeSettings {
  refreshThreshold: number;
  byDifficulty: Record<string, string>;
  escalate: boolean;
  reportMaxChars: number;
  efficiency: { preset: string; voice: string; craft: string; roles: Record<string, unknown>; planLean: boolean };
  routingMode: RoutingState["mode"];
  contextLimit: number;
  providers: unknown[];
  providerKeys: Record<string, string>;
  modelEfforts: Record<string, string>;
  [setting: string]: unknown;
}

export function emptyState(): ReloState {
  return {
    backend: { core: "starting", opencode: "unknown", ready: false, models: [] },
    agents: [],
    tasks: [],
    messages: [],
    changes: [],
    findings: [],
    memory: [],
    verification: [],
    logs: [],
  };
}

// Parses a snapshot/backend `routing` object ({mode, contextLimit}); undefined when malformed.
export function routingFrom(value: unknown): RoutingState | undefined {
  const r = value as Partial<RoutingState> | undefined;
  if (!r || typeof r !== "object") return undefined;
  if (r.mode !== "simple" && r.mode !== "advanced") return undefined;
  const limit = Number(r.contextLimit ?? 0);
  return { mode: r.mode, contextLimit: Number.isFinite(limit) && limit >= 0 ? limit : 0 };
}

export function reduce(state: ReloState, event: string, payload: unknown): ReloState {
  const next = { ...state };
  const p = payload as Record<string, unknown>;
  switch (event) {
    case "backend.status": {
      const incoming = payload as Partial<ReloState["backend"]>;
      next.backend = { ...next.backend, ...incoming };
      const routing = routingFrom({ mode: incoming.routingMode, contextLimit: incoming.contextLimit ?? next.routing?.contextLimit });
      if (routing) next.routing = routing;
      break;
    }
    case "session.created":
    case "session.opened":
      // A different session: nothing from the previous one may stay on screen.
      next.session = payload as ReloState["session"];
      next.agents = [];
      next.tasks = [];
      next.messages = [];
      next.changes = [];
      next.findings = [];
      next.memory = [];
      next.verification = [];
      next.cache = undefined;
      next.inspection = undefined;
      break;
    case "session.updated":
      next.session = payload as ReloState["session"];
      break;
    case "chat.message": {
      const message = payload as ChatMessage;
      const byId = message.id ? next.messages.findIndex((item) => item.id === message.id) : -1;
      const index = byId >= 0 ? byId : streamingIndex(next.messages, message.agentId);
      next.messages = next.messages.slice();
      // The payload is the message's full state: a missing queued flag means it was delivered.
      const queued = message.queued === true;
      if (index >= 0) next.messages[index] = { ...next.messages[index], ...message, streaming: false, queued };
      else next.messages.push({ ...message, streaming: false, queued });
      break;
    }
    case "chat.delta": {
      const agentId = typeof p.agentId === "string" ? p.agentId : "";
      if (!agentId) break;
      const index = streamingIndex(next.messages, agentId);
      const delta = String(p.delta ?? "");
      // text is the cumulative streamed text (idempotent); fall back to appending delta for legacy senders.
      const hasText = typeof p.text === "string";
      const current = index >= 0 ? next.messages[index].text : "";
      let text: string;
      if (hasText) {
        const incoming = p.text as string;
        // Out-of-order older snapshot of the same stream must not shrink the bubble.
        if (incoming.length < current.length && current.startsWith(incoming)) break;
        text = incoming;
      } else {
        text = current + delta;
      }
      if (index < 0) {
        if (!text) break;
        next.messages = [...next.messages, { id: `stream-${agentId}`, role: "assistant", text, agentId, createdAt: new Date().toISOString(), streaming: true }];
      } else {
        next.messages = next.messages.slice();
        next.messages[index] = { ...next.messages[index], text };
      }
      break;
    }
    case "chat.discard": {
      const agentId = typeof p.agentId === "string" ? p.agentId : "";
      if (!agentId) break;
      const index = streamingIndex(next.messages, agentId);
      if (index >= 0) next.messages = next.messages.filter((_, i) => i !== index);
      break;
    }
    case "cache.updated":
      next.cache = normalizeCache(payload);
      break;
    case "agent.created":
    case "agent.started":
    case "agent.completed":
    case "agent.failed":
    case "agent.activity":
    case "agent.updated":
      next.agents = upsertAgent(next.agents, payload as AgentInfo);
      break;
    case "agent.escalated": {
      const patch = (p.agent && typeof p.agent === "object" ? p.agent : p) as AgentEscalatedPayload;
      const id = patch.id || patch.agentId;
      if (id) next.agents = upsertAgent(next.agents, { ...patch, id } as AgentInfo);
      else if (typeof p.taskId === "string") {
        const agent = next.agents.find((item) => item.taskId === p.taskId);
        if (agent) next.agents = upsertAgent(next.agents, { ...agent, model: patch.model || String(p.to ?? agent.model), escalatedFrom: patch.escalatedFrom || String(p.from ?? ""), attempt: patch.attempt });
      }
      break;
    }
    case "task.created":
    case "task.updated":
    case "task.completed":
    case "task.failed":
      next.tasks = upsert(next.tasks, payload as TaskInfo);
      break;
    case "changes.updated":
      next.changes = Array.isArray(payload) ? (payload as ChangeInfo[]) : next.changes;
      break;
    case "finding.created":
      next.findings = [...next.findings, payload];
      break;
    case "memory.updated":
      break;
    case "verification.completed":
      next.verification = [...next.verification, payload];
      break;
    case "log":
      next.logs = [...next.logs.slice(-200), JSON.stringify(payload)];
      break;
    default:
      break;
  }
  if (event.startsWith("agent.")) {
    const agentId = typeof p.id === "string" ? p.id : typeof p.agentId === "string" ? p.agentId : "";
    const agent = agentId ? next.agents.find((a) => a.id === agentId) : undefined;
    if (agent && TERMINAL.has(agent.status)) next.messages = finalizeStream(next.messages, agentId);
  }
  return next;
}

const TERMINAL = new Set<string>(["completed", "failed", "cancelled", "interrupted"]);

function streamingIndex(messages: ChatMessage[], agentId: string | undefined): number {
  if (!agentId) return -1;
  return messages.findIndex((item) => item.streaming && item.role === "assistant" && item.agentId === agentId);
}

// Terminal agent with a streaming bubble that never got chat.message: drop empty bubble, keep non-empty as final.
function finalizeStream(messages: ChatMessage[], agentId: string): ChatMessage[] {
  const index = streamingIndex(messages, agentId);
  if (index < 0) return messages;
  if (messages[index].text) {
    const copy = messages.slice();
    copy[index] = { ...copy[index], streaming: false };
    return copy;
  }
  return messages.filter((_, i) => i !== index);
}

// Upsert for agent.* events. A payload with a numeric rev older than the stored rev is stale and dropped.
// Missing rev always merges. Spread merge lets a full payload clear fields (e.g. phase: "").
function upsertAgent(list: AgentInfo[], item: AgentInfo): AgentInfo[] {
  if (!item || typeof item !== "object" || !item.id) return list;
  const i = list.findIndex((x) => x.id === item.id);
  if (i < 0) return [...list, item];
  const existing = list[i];
  if (typeof item.rev === "number" && typeof existing.rev === "number" && item.rev < existing.rev) return list;
  const copy = list.slice();
  copy[i] = { ...existing, ...item };
  return copy;
}

export const Events = [
  "session.created", "session.opened", "session.updated", "agent.created", "agent.started", "agent.activity", "agent.updated", "agent.completed", "agent.failed", "agent.escalated",
  "context.created", "context.refreshed", "context.compressed", "context.replaced", "context.item.promoted", "context.item.dropped",
  "task.created", "task.updated", "task.completed", "task.failed", "finding.created", "finding.verified", "memory.updated",
  "usage.updated", "changes.updated", "verification.started", "verification.completed", "backend.status", "permission.asked",
  "chat.message", "chat.delta", "chat.discard", "cache.updated", "log",
] as const;

export type EventName = (typeof Events)[number];

function upsert<T extends { id?: string }>(list: T[], item: T): T[] {
  if (!item || typeof item !== "object" || !item.id) {
    if (item && typeof item === "object" && "id" in (item as object)) {
      /* keep */
    } else {
      return list;
    }
  }
  const id = item.id;
  if (!id) return list;
  const i = list.findIndex((x) => x.id === id);
  if (i < 0) return [...list, item];
  const copy = list.slice();
  copy[i] = { ...copy[i], ...item };
  return copy;
}

export function normalizeCache(value: unknown): CacheInfo {
  const raw = value as { session?: Record<string, unknown>; agents?: Record<string, unknown>[] };
  const row = (source: Record<string, unknown> = {}): CacheAgentInfo => {
    const usage = (source.usage ?? {}) as Record<string, number>;
    return {
      agentId: String(source.agentId ?? ""),
      generation: Number(source.generation ?? 0),
      model: String(source.model ?? ""),
      input: Number(source.input ?? usage.input ?? 0),
      output: Number(source.output ?? usage.output ?? 0),
      cacheRead: Number(source.cacheRead ?? usage.cacheRead ?? 0),
      cacheWrite: Number(source.cacheWrite ?? usage.cacheWrite ?? 0),
      hitRate: Number(source.hitRate ?? 0),
    };
  };
  return { session: row(raw?.session), agents: (raw?.agents ?? []).map(row) };
}
