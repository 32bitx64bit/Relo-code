import * as vscode from "vscode";
import * as fs from "node:fs";
import * as path from "node:path";
import { repositoryInfo, snapRepository } from "./repository";
import { CoreClient } from "./client";
import { emptyState, reduce, routingFrom, type ReloState, type AgentInfo, type CacheInfo, type ChangeDiff, type SessionSummary, normalizeCache } from "./protocol";
import { ChatView, openDashboard, openInspector } from "./webview";
import { checkCommand, checkSessionId, checkSetting, modelsSignature, settingsReply, type SettingsSchema } from "./settingsBridge";
import { applyCollapse, cleanCollapsed, type Collapsed } from "./uiState";
import { CrashBreaker } from "./crashBreaker";
import { PROVIDER_ID_RE, sanitizeDiscovered, sanitizeProviderList, secretKey, validateBaseURL, validateProvider, MAX_PROVIDERS, type ProviderConfig } from "./providers";
import { dirtyPathsFor } from "./dirtyGate";
import { CanvasManager } from "./canvas/manager";

let client: CoreClient;
let state: ReloState = emptyState();
let chat: ChatView;
let dashboard: vscode.WebviewPanel | undefined;
let status: vscode.StatusBarItem;
let output: vscode.OutputChannel;

// The output channel is disposed before the core's last words arrive when the window closes; writing to it
// then throws "Channel has been closed" out of an event handler, so logging must never throw.
function log(line: string): void {
  try {
    output.appendLine(line);
  } catch {
    /* window is shutting down */
  }
}
let secrets: vscode.SecretStorage;
let uiMemento: vscode.Memento;
let applyTimer: ReturnType<typeof setTimeout> | undefined;
let extCtx: vscode.ExtensionContext | undefined;
let postedModels = ""; // signature of the model catalog the webview was last sent
const crashes = new CrashBreaker();
let coreLifecycle: Promise<void> = Promise.resolve();
let postedRepository = "";
let canvas: CanvasManager;

// Settings that core consumes; a change to any of them re-applies to the running core.
const CORE_SETTING_SECTIONS = ["relo.routing", "relo.models", "relo.providers", "relo.primaryModel", "relo.tier1Model", "relo.tier2Model", "relo.context", "relo.changes.autoApply", "relo.changes.lazyConflicts", "relo.changes.dependentSemantics", "relo.changes.ignorePaths", "relo.changes.unionPaths", "relo.changes.takeLatestPaths", "relo.scheduler.pathLeases", "relo.workspace", "relo.claudeCode"];

export async function activate(ctx: vscode.ExtensionContext): Promise<void> {
  output = vscode.window.createOutputChannel("Relo");
  extCtx = ctx;
  secrets = ctx.secrets;
  uiMemento = ctx.globalState;
  client = new CoreClient(log);
  canvas = new CanvasManager(workspacePath);
  chat = new ChatView(ctx, client, () => state, (m) => onWebviewMessage(ctx, m));
  status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
  status.command = "relo.open";
  status.text = "$(hubot) Relo: Starting";
  status.show();

  ctx.subscriptions.push(
    status, output, canvas,
    { dispose: () => chat.dispose() },
    vscode.window.registerWebviewViewProvider(ChatView.viewType, chat, { webviewOptions: { retainContextWhenHidden: true } }),
    vscode.workspace.registerTextDocumentContentProvider(DIFF_SCHEME, diffProvider),
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (!e.affectsConfiguration("relo")) return;
      void resendSettings(ctx);
      if (e.affectsConfiguration("relo.repository")) {
        void restartRepositoryCore(ctx).catch((err) => vscode.window.showErrorMessage(`Relo: ${errorText(err)}`));
        return;
      }
      if (CORE_SETTING_SECTIONS.some((section) => e.affectsConfiguration(section))) scheduleCoreApply();
    }),
    vscode.workspace.onDidChangeTextDocument(() => scheduleDirtyPush()),
    vscode.workspace.onDidSaveTextDocument(() => scheduleDirtyPush()),
    vscode.workspace.onDidOpenTextDocument(() => scheduleDirtyPush()),
    vscode.workspace.onDidCloseTextDocument(() => scheduleDirtyPush()),
  );

  client.on("event", (method: string, params: unknown) => {
    if (method === "canvas.requested") {
      const requestId = (params as { requestId?: unknown } | undefined)?.requestId;
      if (typeof requestId !== "string" || requestId.length > 200) return;
      void canvas.handle(params, state.session?.id).then(
        (result) => client.request("canvas.reply", { requestId, result }),
        (error: unknown) => client.request("canvas.reply", { requestId, error: errorText(error) }),
      ).catch((error: unknown) => log(`canvas.reply failed: ${errorText(error)}`));
      return;
    }
    state = reduce(state, method, params);
    chat.post();
    dashboard?.webview.postMessage({ type: "state", state });
    updateStatus();
    if (method === "backend.status") syncModels();
    if (method === "permission.asked") void askPermission(params as { id: string; title: string; command: string });
    if (method === "agent.escalated") {
      const p = params as { from?: string; to?: string; fromModel?: string; model?: string; attempt?: number; agent?: AgentInfo };
      const from = p.fromModel || p.from || "previous model";
      const to = p.model || p.to || p.agent?.model || "new model";
      void vscode.window.showInformationMessage(`Relo escalated agent to ${to} from ${from} (attempt ${p.attempt || p.agent?.attempt || 2})`);
    }
    if (method === "log") log(JSON.stringify(params));
  });
  client.on("error", (err: Error) => {
    state.backend = { ...state.backend, ready: false, error: err.message };
    status.text = "$(error) Relo: Failed";
    void vscode.window.showErrorMessage(`Relo core failed: ${err.message}`);
  });
  client.on("exit", (info: { code: number | null; intentional: boolean }) => {
    status.text = "$(error) Relo: Stopped";
    log(`core exited ${info.code ?? ""}`);
    if (!info.intentional) {
      state.backend = { ...state.backend, ready: false, error: "Relo Core exited unexpectedly." };
      updateStatus();
      if (!crashes.record()) giveUpOnCore(ctx);
      else void restartAfterCrash(ctx);
    }
  });

  registerCommands(ctx);
  const repoTimer = setInterval(() => { void refreshRepository(ctx); }, 5000);
  ctx.subscriptions.push({ dispose: () => clearInterval(repoTimer) });
  await startCore(ctx);
}

export async function deactivate(): Promise<void> {
  canvas?.dispose();
  await client?.dispose();
}

function registerCommands(ctx: vscode.ExtensionContext): void {
  // A command that throws would otherwise do nothing visible; say what went wrong instead.
  const cmd = (id: string, fn: (...args: unknown[]) => unknown) => {
    ctx.subscriptions.push(vscode.commands.registerCommand(id, async (...args: unknown[]) => {
      try {
        return await fn(...args);
      } catch (err) {
        log(`${id} failed: ${errorText(err)}`);
        void vscode.window.showErrorMessage(`Relo: ${errorText(err)}`);
        return undefined;
      }
    }));
  };
  cmd("relo.open", () => vscode.commands.executeCommand("relo.chat.focus"));
  cmd("relo.canvas.open", () => canvas.pickAndOpen());
  cmd("relo.canvas.popout", () => canvas.execute({ action: "popout" }));
  cmd("relo.canvas.capture", () => canvas.saveCapture());
  cmd("relo.canvas.inspect", () => canvas.execute({ action: "inspect" }));
  cmd("relo.newSession", newSession);
  cmd("relo.sessionHistory", async () => {
    chat.reveal();
    chat.send({ type: "page", name: "history" });
    await postSessions();
  });
  cmd("relo.dashboard", async () => {
    dashboard = await openDashboard(ctx, client, () => state, (m) => onDash(m));
  });
  cmd("relo.runTask", () => promptUser("build"));
  cmd("relo.planTask", async () => {
    await client.request("mode.set", { mode: "plan" });
    await promptUser("plan");
  });
  cmd("relo.reviewChanges", async () => {
    await client.request("mode.set", { mode: "review" });
    await client.request("prompt.submit", { text: "Review the current Relo changes independently. Do not mutate unrelated files." });
  });
  cmd("relo.openAgents", () => openDrawer("agents"));
  cmd("relo.openTasks", () => openDrawer("tasks"));
  cmd("relo.openContext", () => openDrawer("context"));
  cmd("relo.openChanges", () => openDrawer("changes"));
  cmd("relo.openVerification", () => openDrawer("checks"));
  cmd("relo.openLogs", () => output.show(true));
  cmd("relo.pause", () => client.request("orchestrator.pause", {}));
  cmd("relo.resume", () => client.request("orchestrator.resume", {}));
  cmd("relo.stopAll", () => client.request("orchestrator.stopAll", {}));
  cmd("relo.refreshPrimary", async () => {
    const primary = state.agents.find((a) => a.role === "primary");
    if (primary) await client.request("agent.refreshContext", { id: primary.id, reason: "manual" });
  });
  cmd("relo.selectPrimaryModel", () => pickModel("primary"));
  cmd("relo.selectTier1Model", () => pickModel("tier1"));
  cmd("relo.selectTier2Model", () => pickModel("tier2"));
  cmd("relo.configureDifficultyRouting", configureDifficultyRouting);
  cmd("relo.showCacheStats", async () => {
    const cache = await client.request<CacheInfo>("cache.get", {});
    state = reduce(state, "cache.updated", cache);
    updateStatus();
    const rows = (state.cache?.agents ?? []).map((a) => `<tr><td>${escapeHtml(a.agentId)}</td><td>${escapeHtml(a.model)}</td><td>${a.generation}</td><td>${a.input}</td><td>${a.cacheRead}</td><td>${a.cacheWrite}</td><td>${(a.hitRate * 100).toFixed(1)}%</td></tr>`).join("");
    const panel = vscode.window.createWebviewPanel("relo.cacheStats", "Relo: Cache Stats", vscode.ViewColumn.Active, { enableScripts: false });
    panel.webview.html = `<!doctype html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>body{font-family:var(--vscode-font-family);color:var(--vscode-foreground);padding:1rem}.table-wrap{overflow-x:auto}table{border-collapse:collapse;width:100%;min-width:40rem}th,td{border:1px solid var(--vscode-panel-border);padding:.4rem;text-align:left}th{background:var(--vscode-editor-background)}</style></head><body><h2>Cache usage by agent</h2><div class="table-wrap"><table><thead><tr><th>Agent</th><th>Model</th><th>Generation</th><th>Input</th><th>Cache read</th><th>Cache write</th><th>Hit rate</th></tr></thead><tbody>${rows || "<tr><td colspan=\"7\">No cache usage recorded</td></tr>"}</tbody></table></div></body></html>`;
  });
  cmd("relo.restartBackend", async () => {
    crashes.reset();
    if (!client.running || !client.ready) {
      await startCore(ctx);
      return;
    }
    await client.request("backend.restart", {});
    const st = await client.request("backend.status", {});
    state = reduce(state, "backend.status", st);
    updateStatus();
    syncModels();
  });
  cmd("relo.settings", () => openSettingsPage());
  cmd("relo.selectRepository", () => selectRepository());
  cmd("relo.askSelection", () => fromEditor("Explain this selection and related constraints."));
  cmd("relo.fixSelection", () => fromEditor("Fix the selected code. Stay in scope."));
  cmd("relo.refactorSelection", () => fromEditor("Refactor the selection without changing behavior."));
  cmd("relo.reviewSelection", () => fromEditor("Review the selection for defects and risks."));
  cmd("relo.askFile", (uri?: unknown) => fromUri(uri as vscode.Uri | undefined, "Explain this file and how Relo should treat it."));
  cmd("relo.reviewFile", (uri?: unknown) => fromUri(uri as vscode.Uri | undefined, "Review this file."));
  cmd("relo.askFolder", (uri?: unknown) => fromUri(uri as vscode.Uri | undefined, "Explain this folder's role in the project."));
  // Row commands receive {id} from the webview (see settingsBridge.checkCommand) or a legacy item with id.
  cmd("relo.agent.stop", (item?: unknown) => {
    const id = idOf(item);
    if (id) void client.request("agent.stop", { id }).catch(() => {});
  });
  cmd("relo.agent.refresh", (item?: unknown) => {
    const id = idOf(item);
    if (id) void client.request("agent.refreshContext", { id, reason: "manual" }).catch(() => {});
  });
  cmd("relo.agent.inspect", async (item?: unknown) => {
    const id = idOf(item) || state.agents[0]?.id;
    if (!id) return;
    const ins = await client.request("agent.inspectContext", { id });
    await openInspector(ctx, ins);
  });
  cmd("relo.agent.openWorktree", async (item?: unknown) => {
    const id = idOf(item);
    if (!id) return;
    const res = await client.request<{ path: string }>("agent.openWorktree", { id });
    if (res?.path) {
      const uri = vscode.Uri.file(res.path);
      await vscode.commands.executeCommand("revealInExplorer", uri);
    }
  });
  cmd("relo.changes.accept", (item?: unknown) => decideChange("accept", idOf(item)));
  cmd("relo.changes.acceptWithParents", (item?: unknown) => acceptWithParents(idOf(item)));
  cmd("relo.changes.reject", (item?: unknown) => decideChange("reject", idOf(item)));
  cmd("relo.changes.rejectWithDependents", (item?: unknown) => rejectWithDependents(idOf(item)));
  cmd("relo.changes.rebaseWithoutParent", (item?: unknown) => rebaseWithoutParent(idOf(item)));
  cmd("relo.changes.resolveAgent", (item?: unknown) => resolveChange("agent", idOf(item)));
  cmd("relo.changes.resolveMine", (item?: unknown) => resolveChange("mine", idOf(item)));
  cmd("relo.changes.resolveHead", (item?: unknown) => resolveChange("head", idOf(item)));
  cmd("relo.changes.acceptAll", () => decideAll("accept"));
  cmd("relo.changes.rejectAll", () => decideAll("reject"));
  cmd("relo.changes.undo", (item?: unknown) => undoChange(idOf(item)));
  cmd("relo.changes.diff", (item?: unknown) => openChangeDiff(idOf(item)));
  cmd("relo.changes.resolveConflicts", () => resolveConflicts());
}

function idOf(item: unknown): string | undefined {
  const id = (item as { id?: unknown } | undefined)?.id;
  return typeof id === "string" && id ? id : undefined;
}

// Webview -> extension messages beyond submit/cancel/mode. Everything is validated before use.
function onWebviewMessage(ctx: vscode.ExtensionContext, m: unknown): void {
  const msg = (m ?? {}) as { type?: string; key?: unknown; value?: unknown; command?: unknown; id?: unknown; provider?: unknown; apiKey?: unknown; requestId?: unknown };
  const schema = settingsSchema(ctx);
  switch (msg.type) {
    case "settings.get":
      void postSettings(schema);
      return;
    case "settings.set": {
      const check = checkSetting(msg.key, msg.value, schema);
      if (!check.ok) return;
      // Providers are re-validated: a webview payload must not smuggle extra fields (e.g. apiKey) into settings.
      const value = check.subkey === "providers" ? sanitizeProviderList(check.value).providers : check.value;
      void vscode.workspace.getConfiguration("relo").update(check.subkey, value, settingTarget(check.subkey)).then(() => postSettings(schema), (err) => log(`settings.set failed: ${String(err)}`));
      return;
    }
    case "settings.reset": {
      const check = checkSetting(msg.key, schema[String(msg.key)]?.default, schema);
      if (!check.ok) return;
      void vscode.workspace.getConfiguration("relo").update(check.subkey, undefined, settingTarget(check.subkey)).then(() => postSettings(schema), (err) => log(`settings.reset failed: ${String(err)}`));
      return;
    }
    case "ui.collapse": {
      const before = readCollapsed();
      const next = applyCollapse(before, msg.key, (m as { collapsed?: unknown }).collapsed);
      if (next !== before) void uiMemento.update(COLLAPSE_STATE_KEY, next);
      return;
    }
    case "provider.save":
      void saveProvider(msg.provider, msg.apiKey, schema).catch((err) => log(`provider.save failed: ${errorText(err)}`));
      return;
    case "provider.remove":
      void removeProvider(msg.id, schema).catch((err) => log(`provider.remove failed: ${errorText(err)}`));
      return;
    case "provider.discover":
      void discoverProvider(msg as DiscoverMessage).catch((err) => chat.send({ type: "provider.discovered", requestId: requestIdOf(msg), models: [], error: errorText(err) }));
      return;
    case "command": {
      const check = checkCommand({ command: msg.command, id: msg.id });
      if (!check.ok) return;
      void vscode.commands.executeCommand(check.command, ...check.args);
      return;
    }
    case "sessions.list":
      void postSessions();
      return;
    case "session.open": {
      const id = checkSessionId(msg.id);
      if (id) void openSession(id).catch((err) => void vscode.window.showErrorMessage(`Relo: could not open the session: ${errorText(err)}`));
      return;
    }
    case "session.delete": {
      const id = checkSessionId(msg.id);
      if (id) void deleteSession(id).catch((err) => void vscode.window.showErrorMessage(`Relo: could not delete the session: ${errorText(err)}`));
      return;
    }
    case "settings.open":
      void vscode.commands.executeCommand("workbench.action.openSettings", "@ext:relocode.relo-code");
      return;
    default:
      return;
  }
}

type DiscoverMessage = { kind?: unknown; baseURL?: unknown; apiKey?: unknown; id?: unknown; requestId?: unknown };

function requestIdOf(msg: { requestId?: unknown }): string | undefined {
  return typeof msg.requestId === "string" && msg.requestId.length <= 200 ? msg.requestId : undefined;
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// Typed API key from the webview: undefined when absent/empty (keep stored key), throws when malformed.
function apiKeyFrom(value: unknown): string | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "string") throw new Error("apiKey must be a string");
  const key = value.trim();
  if (key.length > 4096) throw new Error("apiKey too long");
  return key || undefined;
}

// Writes the provider list (sanitized) to Global settings. Keys are never written here.
async function writeProviders(list: ProviderConfig[]): Promise<void> {
  await cfg().update("providers", list, vscode.ConfigurationTarget.Global);
}

async function saveProvider(raw: unknown, apiKeyRaw: unknown, schema: SettingsSchema): Promise<void> {
  const check = validateProvider(raw);
  if (!check.ok) {
    log(`provider.save rejected: ${check.reason}`);
    return;
  }
  const apiKey = apiKeyFrom(apiKeyRaw);
  const current = currentProviders();
  const id = check.provider.id;
  const candidate = current.some((p) => p.id === id) ? current.map((p) => (p.id === id ? check.provider : p)) : [...current, check.provider];
  if (candidate.length > MAX_PROVIDERS) {
    log(`provider.save rejected: at most ${MAX_PROVIDERS} providers`);
    return;
  }
  const result = sanitizeProviderList(candidate);
  if (!result.providers.some((p) => p.id === id)) {
    log(`provider.save rejected: ${result.errors.join("; ")}`);
    return;
  }
  if (apiKey) await secrets.store(secretKey(id), apiKey);
  await writeProviders(result.providers);
  await postSettings(schema);
  await applyCoreSettings();
}

async function removeProvider(rawId: unknown, schema: SettingsSchema): Promise<void> {
  if (typeof rawId !== "string" || !PROVIDER_ID_RE.test(rawId)) {
    log("provider.remove rejected: invalid id");
    return;
  }
  const current = currentProviders();
  await writeProviders(current.filter((p) => p.id !== rawId));
  await secrets.delete(secretKey(rawId));
  await postSettings(schema);
  await applyCoreSettings();
}

// Calls core providers.discover. Failures become a reply error; the apiKey is never echoed back.
async function discoverProvider(msg: DiscoverMessage): Promise<void> {
  const requestId = requestIdOf(msg);
  const reply = (models: unknown[], error: string) => chat.send({ type: "provider.discovered", requestId, models, error });
  if (msg.kind !== "openai" && msg.kind !== "anthropic") return reply([], "kind must be openai or anthropic");
  const base = validateBaseURL(msg.baseURL);
  if (!base.ok) return reply([], base.reason);
  let apiKey = apiKeyFrom(msg.apiKey);
  if (!apiKey && typeof msg.id === "string" && PROVIDER_ID_RE.test(msg.id)) apiKey = await secrets.get(secretKey(msg.id));
  if (!client.ready) return reply([], "Relo core is not running");
  try {
    const res = await client.request<{ models?: unknown[]; error?: unknown }>("providers.discover", { kind: msg.kind, baseURL: base.url, apiKey: apiKey ?? "" });
    reply(sanitizeDiscovered(res?.models), typeof res?.error === "string" ? res.error : "");
  } catch (err) {
    reply([], errorText(err));
  }
}

// Re-applies settings to the running core (providers/keys/routing/limits). Skipped silently when core is not ready.
async function applyCoreSettings(): Promise<void> {
  if (!client?.ready) return;
  try {
    const res = await client.request<{ backend?: unknown; restartPending?: boolean }>("settings.apply", await settingsPayload(workspacePath()));
    if (res?.backend) state = reduce(state, "backend.status", res.backend);
    if (res?.restartPending) log("settings applied; backend restart pending until agents are idle");
    warnClaudeCode();
    updateStatus();
    chat.post();
    syncModels();
  } catch (err) {
    log(`settings.apply failed: ${errorText(err)}`);
  }
}

// warnClaudeCode says once why Claude Code's models are missing when relo.claudeCode.enabled is on but the CLI could
// not be used (not found, or signed out). The same reason is not repeated on every settings change.
let claudeCodeWarned = "";
function warnClaudeCode(): void {
  const cc = state.backend.claudeCode;
  const problem = cc?.enabled && !cc.ready && cc.error ? cc.error : "";
  if (problem && problem !== claudeCodeWarned) void vscode.window.showWarningMessage(`Claude Code is not available to Relo: ${problem}`);
  claudeCodeWarned = problem;
}

function scheduleCoreApply(): void {
  if (applyTimer) clearTimeout(applyTimer);
  applyTimer = setTimeout(() => {
    applyTimer = undefined;
    void applyCoreSettings();
  }, 300);
}

const COLLAPSE_STATE_KEY = "relo.ui.collapsed";

function readCollapsed(): Collapsed {
  return cleanCollapsed(uiMemento.get<unknown>(COLLAPSE_STATE_KEY));
}

function currentProviders(): ProviderConfig[] {
  return sanitizeProviderList(cfg().get<unknown>("providers", [])).providers;
}

function workspacePath(): string {
  const selected = cfg().get<string>("repository", "");
  return selected ? path.resolve(selected) : vscode.workspace.workspaceFolders?.[0]?.uri.fsPath || process.cwd();
}

function settingTarget(subkey: string): vscode.ConfigurationTarget {
  return (subkey === "repository" || subkey === "changes.autoApply" || subkey === "changes.lazyConflicts" || subkey === "changes.ignorePaths" || subkey === "changes.unionPaths" || subkey === "changes.takeLatestPaths") && vscode.workspace.workspaceFolders?.length
    ? vscode.ConfigurationTarget.Workspace : vscode.ConfigurationTarget.Global;
}

async function selectRepository(): Promise<void> {
  const folders = vscode.workspace.workspaceFolders || [];
  const roots = new Set(folders.filter((f) => f.uri.scheme === "file").map((f) => f.uri.fsPath));
  roots.add(workspacePath());
  const gitExtension = vscode.extensions.getExtension<{ getAPI(version: number): { repositories: { rootUri: vscode.Uri }[] } }>("vscode.git");
  try {
    const api = (await gitExtension?.activate())?.getAPI(1);
    for (const repo of api?.repositories || []) if (repo.rootUri.scheme === "file") roots.add(repo.rootUri.fsPath);
  } catch (err) {
    log(`repository discovery: ${errorText(err)}`);
  }
  const found = await Promise.all([...roots].map(repositoryInfo));
  for (const repo of found) if (repo.gitRoot) roots.add(repo.gitRoot);
  const items = [...roots].map((root) => ({ label: path.basename(root), description: root, root }));
  const picked = await vscode.window.showQuickPick([
    { label: "Default workspace folder", description: folders[0]?.uri.fsPath || process.cwd(), root: "" },
    ...items,
  ], { title: "Relo: Select Repository", placeHolder: "Apply changes to this folder's checked-out branch", matchOnDescription: true });
  if (!picked) return;
  const root = await snapRepository(picked.root, "");
  if (root === undefined) {
    void vscode.window.showErrorMessage(`Relo: ${picked.root} is not inside a Git repository. Repository setting unchanged.`);
    return;
  }
  await cfg().update("repository", root, settingTarget("repository"));
}

// Resolves relo.repository for core launch; undefined when the setting points outside a Git repository.
async function launchRepository(): Promise<string | undefined> {
  const selected = cfg().get<string>("repository", "");
  const root = await snapRepository(selected, workspacePath());
  if (root === undefined) void vscode.window.showErrorMessage(`Relo: ${selected} is not inside a Git repository. Fix relo.repository to start the backend.`);
  return root;
}

async function refreshRepository(ctx: vscode.ExtensionContext): Promise<void> {
  const repository = await repositoryInfo(workspacePath());
  if (JSON.stringify(repository) !== postedRepository) await postSettings(settingsSchema(ctx));
}

// Reads stored API keys for the configured providers. Returns id -> key (only ids with a non-empty key).
async function readProviderKeys(providers: ProviderConfig[]): Promise<Record<string, string>> {
  const keys: Record<string, string> = {};
  for (const p of providers) {
    const key = await secrets.get(secretKey(p.id)).then((v) => v, () => undefined);
    if (key) keys[p.id] = key;
  }
  return keys;
}

function settingsSchema(ctx: vscode.ExtensionContext): SettingsSchema {
  const props = (ctx.extension.packageJSON as { contributes?: { configuration?: { properties?: SettingsSchema } } }).contributes?.configuration?.properties ?? {};
  return props;
}

// Sends settings to the webview. providerKeys carries presence flags only, never the key values.
async function postSettings(schema: SettingsSchema): Promise<void> {
  const c = cfg();
  const values: Record<string, unknown> = {};
  for (const key of Object.keys(schema)) {
    if (key.startsWith("relo.")) values[key] = c.get(key.slice("relo.".length));
  }
  const providers = currentProviders();
  values["relo.providers"] = providers;
  const keys = await readProviderKeys(providers);
  const providerKeys: Record<string, boolean> = {};
  for (const p of providers) providerKeys[p.id] = Boolean(keys[p.id]);
  postedModels = modelsSignature(state.backend.models);
  const repository = await repositoryInfo(workspacePath());
  postedRepository = JSON.stringify(repository);
  chat.send({ ...settingsReply(values, schema, state.backend.models || []), providerKeys, collapsed: readCollapsed(), repository });
}

// The pickers in the webview read their models from the settings message. That message is built the moment a
// setting changes, which is before the core has re-read the provider config and restarted OpenCode, so a model the
// user had just added was missing from it and stayed missing until the window was reloaded. Send it again when the
// core reports a different catalog.
function syncModels(): void {
  if (!extCtx || modelsSignature(state.backend.models) === postedModels) return;
  void resendSettings(extCtx);
}

// A settings change refreshes the webview here and re-applies to a running core (see scheduleCoreApply).
function resendSettings(ctx: vscode.ExtensionContext): Promise<void> {
  return postSettings(settingsSchema(ctx));
}

function openDrawer(name: string): void {
  chat.reveal();
  chat.send({ type: "drawer", name });
}

function openSettingsPage(): void {
  chat.reveal();
  chat.send({ type: "page", name: "settings" });
}

function onDash(m: unknown): void {
  const msg = m as { type: string; text?: string; mode?: string };
  if (msg.type === "submit" && msg.text) void client.request("prompt.submit", { text: msg.text }).catch((err) => void vscode.window.showErrorMessage(`Relo: prompt failed: ${errorText(err)}`));
  if (msg.type === "cancel") void client.request("prompt.cancel", {}).catch(() => {});
  if (msg.type === "mode" && msg.mode) void client.request("mode.set", { mode: msg.mode }).catch(() => {});
}

async function promptUser(mode: string): Promise<void> {
  const text = await vscode.window.showInputBox({ prompt: mode === "plan" ? "What should Relo plan?" : "What should Relo build?", ignoreFocusOut: true });
  if (!text) return;
  await client.request("mode.set", { mode });
  await client.request("prompt.submit", { text });
}

async function fromEditor(instruction: string): Promise<void> {
  const ed = vscode.window.activeTextEditor;
  if (!ed) return;
  const sel = ed.document.getText(ed.selection) || ed.document.getText();
  const rel = vscode.workspace.asRelativePath(ed.document.uri);
  await client.request("prompt.submit", { text: `${instruction}\n\nFile: ${rel}\n\n${sel}`, refs: [rel] });
}

async function fromUri(uri: vscode.Uri | undefined, instruction: string): Promise<void> {
  const target = uri || vscode.window.activeTextEditor?.document.uri;
  if (!target) return;
  const rel = vscode.workspace.asRelativePath(target);
  await client.request("prompt.submit", { text: `${instruction}\n\nPath: ${rel}`, refs: [rel] });
}

async function pickModel(which: "primary" | "tier1" | "tier2"): Promise<void> {
  const models = state.backend.models || [];
  if (!models.length) {
    void vscode.window.showInformationMessage("Relo has no models yet. Add a provider in Relo Settings first.", "Open Settings").then((c) => {
      if (c) openSettingsPage();
    });
    return;
  }
  const items = models.map((m) => ({ label: `${m.provider}/${m.id}`, description: m.name }));
  const picked = await vscode.window.showQuickPick(items, { title: `Select ${which} model` });
  if (!picked) return;
  const body: Record<string, string> = {};
  body[which] = picked.label;
  await client.request("models.set", body);
  await writeSetting(which === "primary" ? "primaryModel" : which === "tier1" ? "tier1Model" : "tier2Model", picked.label);
}

// Model choices are user-level, like everything on the settings page. A leftover workspace value would silently
// override them, so it is cleared too.
async function writeSetting(key: string, value: unknown): Promise<void> {
  const config = cfg();
  await config.update(key, value, vscode.ConfigurationTarget.Global);
  if (config.inspect(key)?.workspaceValue !== undefined) await config.update(key, undefined, vscode.ConfigurationTarget.Workspace);
}

async function askPermission(p: { id: string; title: string; command: string }): Promise<void> {
  const choice = await vscode.window.showWarningMessage(`Relo needs approval: ${p.title || p.command}`, { modal: true }, "Allow once", "Always", "Reject");
  const response = choice === "Allow once" ? "once" : choice === "Always" ? "always" : "reject";
  await client.request("permission.respond", { id: p.id, response }).catch(() => {});
}

async function startCore(ctx: vscode.ExtensionContext, maxAttempts = 5, showFailure = true): Promise<void> {
  coreLifecycle = coreLifecycle.catch(() => {}).then(() => startCoreNow(ctx, maxAttempts, showFailure));
  return coreLifecycle;
}

function restartRepositoryCore(ctx: vscode.ExtensionContext): Promise<void> {
  coreLifecycle = coreLifecycle.catch(() => {}).then(async () => {
    if (applyTimer) clearTimeout(applyTimer);
    await client.dispose();
    canvas.dispose();
    canvas = new CanvasManager(workspacePath);
    ctx.subscriptions.push(canvas);
    state = emptyState();
    chat.post();
    crashes.reset();
    await startCoreNow(ctx);
    await resendSettings(ctx);
  });
  return coreLifecycle;
}

async function startCoreNow(ctx: vscode.ExtensionContext, maxAttempts = 5, showFailure = true): Promise<void> {
  const ws = await launchRepository();
  if (ws === undefined) {
    status.text = "$(error) Relo: Invalid repository";
    return;
  }
  for (let attempt = 0; attempt < maxAttempts; attempt++) {
   if (client.running) {
    if (client.ready) return;
    await client.dispose();
   }
   try {
    const bin = locateCore(ctx, ws);
    const args = ["-workspace", ws, "-runtime", cfg().get<string>("runtime") || "opencode", "-log-level", cfg().get<string>("logLevel") || "info"];
    const oc = cfg().get<string>("openCode.path");
    if (oc) args.push("-opencode", oc);
    if (bin.cmd === "go") client.start("go", ["run", "./cmd/relo-core", ...args], bin.cwd);
    else client.start(bin.cmd, args, ws);

    const init = await client.request<Record<string, unknown>>("initialize", await settingsPayload(ws));
    client.ready = true;
    if (init.backend) state = reduce(state, "backend.status", init.backend);
    if (init.session) state.session = init.session as ReloState["session"];
    const snap = await client.request<Record<string, unknown>>("snapshot", {});
    applySnapshot(snap);
    const cache = await client.request<CacheInfo>("cache.get", {}).catch(() => undefined);
    if (cache) state = reduce(state, "cache.updated", cache);
    if (!(state.backend.primary || cfg().get("primaryModel")) && (state.backend.models || []).length) {
      // Never block startup on a picker: the user may not be looking at VS Code yet.
      void vscode.window.showInformationMessage("Relo needs a Primary model to plan work.", "Choose Primary model").then((c) => {
        if (c) void vscode.commands.executeCommand("relo.selectPrimaryModel");
      });
    }
    updateStatus();
    syncModels();
    if (state.backend.error) {
      void vscode.window.showWarningMessage(String(state.backend.error));
    }
    warnClaudeCode();
   } catch (err) {
    if (attempt < maxAttempts - 1) {
      await new Promise((resolve) => setTimeout(resolve, 500 * 2 ** attempt));
      continue;
    }
    status.text = "$(error) Relo: Failed";
    const message = err instanceof Error ? err.message : String(err);
    if (showFailure) void vscode.window.showErrorMessage(`Relo failed to start after ${maxAttempts} attempt${maxAttempts === 1 ? "" : "s"}: ${message}`, "Restart Backend").then((choice) => {
      if (choice === "Restart Backend") {
        crashes.reset();
        void startCore(ctx);
      }
    });
    return;
   }
   return;
  }
}

// One recovery loop at a time: a core that dies during a restart attempt fires another exit event.
let recovering = false;

async function restartAfterCrash(ctx: vscode.ExtensionContext): Promise<void> {
  if (recovering) return;
  recovering = true;
  try {
    for (let attempt = 0; attempt < 5; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 500 * 2 ** attempt));
      if (crashes.tripped) return;
      try {
        await startCore(ctx, 1, false);
        if (client.ready) return;
      } catch (err) {
        log(`core restart attempt ${attempt + 1} failed: ${String(err)}`);
      }
    }
  } finally {
    recovering = false;
  }
  if (!crashes.tripped) giveUpOnCore(ctx);
}

// Stops restarting; the user decides when to try again (which also clears the crash history).
function giveUpOnCore(ctx: vscode.ExtensionContext): void {
  state.backend = { ...state.backend, ready: false, error: "Relo Core keeps crashing; automatic restarts are paused." };
  status.text = "$(error) Relo: Failed";
  updateStatus();
  void vscode.window.showErrorMessage("Relo Core crashed repeatedly and was not restarted again.", "Show Logs", "Restart Backend").then((choice) => {
    if (choice === "Show Logs") output.show();
    if (choice === "Restart Backend") {
      crashes.reset();
      void startCore(ctx);
    }
  });
}

function locateCore(ctx: vscode.ExtensionContext, ws: string): { cmd: string; cwd?: string } {
  const osName = process.platform === "win32" ? "windows" : process.platform;
  const arch = process.arch === "x64" ? "amd64" : process.arch;
  const ext = process.platform === "win32" ? ".exe" : "";
  const name = `relo-core-${osName}-${arch}${ext}`;
  const bundled = path.join(ctx.extensionPath, "bin", name);
  if (fs.existsSync(bundled)) return { cmd: bundled };
  const local = path.join(ctx.extensionPath, "..", "core");
  if (fs.existsSync(path.join(local, "cmd", "relo-core"))) return { cmd: "go", cwd: local };
  const repo = path.join(ws, "core", "cmd", "relo-core");
  if (fs.existsSync(repo)) return { cmd: "go", cwd: path.join(ws, "core") };
  throw new Error("Relo Core binary not found. Run `make core` from the Relo repository.");
}

// Payload for initialize and settings.apply. Includes provider API keys, which go to core only.
async function settingsPayload(workspace: string): Promise<Record<string, unknown>> {
  const c = cfg();
  const providers = currentProviders();
  return {
    workspace,
    canvas: true,
    autoApply: c.get<boolean>("changes.autoApply", false),
    lazyConflicts: c.get<boolean>("changes.lazyConflicts", false),
    dependentSemantics: c.get<boolean>("changes.dependentSemantics", false),
    ignorePaths: c.get<string[]>("changes.ignorePaths", [".relo/**", "**/obj/**", "**/bin/Debug/**", "**/bin/Release/**"]),
    unionPaths: c.get<string[]>("changes.unionPaths", ["*.md", "*.txt"]),
    takeLatestPaths: c.get<string[]>("changes.takeLatestPaths", []),
    pathLeases: c.get<boolean>("scheduler.pathLeases", false),
    workspaceMode: c.get<string>("workspace.mode", "worktree"),
    workspaceMaterialization: c.get<string>("workspace.materialization", "automatic"),
    workspaceVirtualReads: c.get<boolean>("workspace.virtualReads", false),
    routingMode: c.get<string>("routing.mode", "simple"),
    contextLimit: c.get<number>("models.contextLimit", 0),
    providers,
    providerKeys: await readProviderKeys(providers),
    modelEfforts: c.get<Record<string, string>>("models.efforts", {}),
    runtime: c.get("runtime"),
    openCodePath: c.get("openCode.path"),
    claudeCode: { enabled: c.get<boolean>("claudeCode.enabled", false), path: c.get<string>("claudeCode.path", "") },
    primaryModel: c.get("primaryModel"),
    tier1Model: c.get("tier1Model"),
    tier2Model: c.get("tier2Model"),
    maxConcurrentAgents: c.get("maxConcurrentAgents"),
    maxTier1Agents: c.get("maxTier1Agents"),
    maxTier2Agents: c.get("maxTier2Agents"),
    maxHierarchyDepth: c.get("maxHierarchyDepth"),
    maxRetries: c.get("maxRetries"),
    maxUtilization: c.get("context.maxUtilization"),
    compactAt: c.get("context.compactAt"),
    refreshThreshold: c.get("context.refreshThreshold"),
    autoRefresh: c.get("context.autoRefresh"),
    keepRawHistory: c.get("context.keepRawHistory"),
    projectMemory: c.get("context.projectMemory"),
    compressionStrategy: c.get("context.compressionStrategy"),
    autoVerify: c.get("autoVerify"),
    autoReview: c.get("autoReview"),
    logLevel: c.get("logLevel"),
    byDifficulty: c.get<Record<string, string>>("routing.byDifficulty", {}),
    escalate: c.get<boolean>("routing.escalate", true),
    reportMaxChars: c.get<number>("report.maxChars", 1200),
    efficiency: {
      preset: c.get("efficiency.preset", "off"),
      voice: c.get("efficiency.voice", "auto"),
      craft: c.get("efficiency.craft", "auto"),
      roles: c.get<Record<string, unknown>>("efficiency.roles", {}),
      planLean: c.get("efficiency.planLean", false),
    },
    permissionRead: c.get("permissions.read"),
    permissionWrite: c.get("permissions.write"),
    permissionTest: c.get("permissions.test"),
    permissionBuild: c.get("permissions.build"),
    permissionInstall: c.get("permissions.install"),
    permissionDelete: c.get("permissions.delete"),
    permissionGitCommit: c.get("permissions.gitCommit"),
    permissionGitPush: c.get("permissions.gitPush"),
    permissionNetwork: c.get("permissions.network"),
    permissionOutside: c.get("permissions.outside"),
    permissionDestructive: c.get("permissions.destructive"),
    permissionYolo: c.get<boolean>("permissions.yolo", false),
  };
}

// A snapshot is the whole truth: a list that is missing or null means empty, never "keep what was there".
function applySnapshot(snap: Record<string, unknown>): void {
  const list = <T>(value: unknown): T[] => (Array.isArray(value) ? (value as T[]) : []);
  if (snap.session) state.session = snap.session as ReloState["session"];
  state.agents = list(snap.agents);
  state.tasks = list(snap.tasks);
  state.messages = list(snap.messages);
  state.changes = list(snap.changes);
  state.findings = list(snap.findings);
  state.memory = list(snap.memory);
  state.verification = list(snap.verification);
  if (snap.cache && typeof snap.cache === "object") state.cache = normalizeCache(snap.cache);
  if (snap.efficiency && typeof snap.efficiency === "object") state.efficiency = snap.efficiency as ReloState["efficiency"];
  const routing = routingFrom(snap.routing);
  if (routing) state.routing = routing;
  updateStatus();
  chat.post();
  dashboard?.webview.postMessage({ type: "state", state });
}

async function newSession(): Promise<void> {
  if (!client.ready) {
    void vscode.window.showWarningMessage("Relo is still starting. Try again in a moment.");
    return;
  }
  if (state.session?.running) {
    const choice = await vscode.window.showWarningMessage("Relo is still working on a request. Starting a new session stops it.", { modal: true }, "Stop and start new");
    if (!choice) return;
  }
  const snap = await client.request<Record<string, unknown>>("session.create", { mode: cfg().get("mode") ?? "build" });
  state = reduce(state, "session.created", snap.session);
  applySnapshot(snap);
  chat.send({ type: "page", name: "chat" });
}

async function postSessions(): Promise<void> {
  try {
    const sessions = await client.request<SessionSummary[]>("session.list", {});
    chat.send({ type: "sessions", sessions: Array.isArray(sessions) ? sessions : [] });
  } catch (err) {
    chat.send({ type: "sessions", sessions: [], error: errorText(err) });
  }
}

async function openSession(id: string): Promise<void> {
  if (state.session?.running) {
    const choice = await vscode.window.showWarningMessage("Relo is still working on a request. Opening another session stops it.", { modal: true }, "Stop and open");
    if (!choice) return;
  }
  const snap = await client.request<Record<string, unknown>>("session.open", { id });
  state = reduce(state, "session.opened", snap.session);
  applySnapshot(snap);
  chat.send({ type: "page", name: "chat" });
}

async function deleteSession(id: string): Promise<void> {
  const choice = await vscode.window.showWarningMessage("Delete this session and its history? Changes that were not accepted are lost.", { modal: true }, "Delete");
  if (choice !== "Delete") return;
  const res = await client.request<{ sessions?: SessionSummary[]; snapshot?: Record<string, unknown> }>("session.delete", { id });
  if (res?.snapshot && (res.snapshot.session as { id?: string } | undefined)?.id !== state.session?.id) {
    state = reduce(state, "session.opened", res.snapshot.session);
    applySnapshot(res.snapshot);
  }
  chat.send({ type: "sessions", sessions: Array.isArray(res?.sessions) ? res.sessions : [] });
}

function relativeDocumentPath(fsPath: string): string | undefined {
  const rel = path.relative(workspacePath(), fsPath);
  if (!rel || rel.startsWith("..") || path.isAbsolute(rel)) return undefined;
  return rel.split(path.sep).join("/");
}

let dirtyTimer: ReturnType<typeof setTimeout> | undefined;

// The core needs the dirty set to refuse an accept or auto-apply it cannot see. Debounced so typing does not flood
// the connection.
function scheduleDirtyPush(): void {
  if (!client) return;
  if (dirtyTimer) clearTimeout(dirtyTimer);
  dirtyTimer = setTimeout(() => void pushDirty(), 250);
}

async function pushDirty(): Promise<void> {
  if (dirtyTimer) {
    clearTimeout(dirtyTimer);
    dirtyTimer = undefined;
  }
  const paths = vscode.workspace.textDocuments
    .filter((doc) => doc.isDirty && !doc.isUntitled)
    .map((doc) => relativeDocumentPath(doc.uri.fsPath))
    .filter((p): p is string => Boolean(p));
  try {
    await client.request("workspace.dirty", { paths });
  } catch {
    /* the core may be restarting; the next edit pushes again */
  }
}

function dirtyDocumentsFor(paths: string[]): vscode.TextDocument[] {
  const docs = vscode.workspace.textDocuments.map((doc) => ({
    doc,
    path: relativeDocumentPath(doc.uri.fsPath) ?? "",
    isDirty: doc.isDirty,
  }));
  const dirty = new Set(dirtyPathsFor(paths, docs));
  return docs.filter((d) => d.isDirty && dirty.has(d.path)).map((d) => d.doc);
}

type DirtyChoice = "Save" | "Skip" | "Cancel";

// Save proceeds, Skip leaves the dirty changes for later (the core refuses them), Cancel aborts the whole action.
async function gateDirty(paths: string[], action: string): Promise<DirtyChoice> {
  const dirty = dirtyDocumentsFor(paths);
  if (!dirty.length) return "Save";
  const names = dirty.slice(0, 3).map((doc) => path.basename(doc.uri.fsPath)).join(", ");
  const more = dirty.length > 3 ? ` and ${dirty.length - 3} more` : "";
  const choice = await vscode.window.showWarningMessage(
    `Relo: ${names}${more} ${dirty.length === 1 ? "has" : "have"} unsaved changes. Save before ${action}?`,
    { modal: true },
    "Save",
    "Skip",
    "Cancel",
  );
  if (choice === "Save") {
    for (const doc of dirty) {
      if (!(await doc.save())) return "Cancel";
    }
    await pushDirty();
    return "Save";
  }
  if (choice === "Skip") {
    await pushDirty();
    return "Skip";
  }
  return "Cancel";
}

async function decideChange(kind: "accept" | "reject", id: string | undefined): Promise<void> {
  if (!id) return;
  try {
    const chg = state.changes.find((c) => c.id === id);
    if (kind === "accept") {
      const choice = await gateDirty(chg?.path ? [chg.path] : [], "accepting it");
      if (choice !== "Save") return;
    }
    const expect = kind === "accept" ? chg?.integration?.fingerprint : undefined;
    await client.request(kind === "accept" ? "changes.accept" : "changes.reject", expect ? { id, expect } : { id });
  } catch (err) {
    // The core's message says which file and why (for example "a.ts: your edits and the agent's edits overlap").
    void vscode.window.showErrorMessage(`Relo: could not ${kind} the change. ${errorText(err)}`);
  }
}

async function acceptWithParents(id: string | undefined): Promise<void> {
  if (!id) return;
  try {
    const chg = state.changes.find((c) => c.id === id);
    const parents = (chg?.needs ?? []).map((need) => state.changes.find((c) => c.id === need)).filter(Boolean);
    const paths = [chg?.path, ...parents.map((p) => p?.path)].filter((p): p is string => !!p);
    const choice = await gateDirty(paths, "accepting the change with its parents");
    if (choice !== "Save") return;
    await client.request("changes.accept", { id, withParents: true });
  } catch (err) {
    void vscode.window.showErrorMessage(`Relo: could not accept the change with its parents. ${errorText(err)}`);
  }
}

async function rebaseWithoutParent(id: string | undefined): Promise<void> {
  if (!id) return;
  const chg = state.changes.find((c) => c.id === id);
  const ok = await vscode.window.showWarningMessage(
    `Rebase ${chg?.path ?? "this change"} without the parent it was built on? The change was authored and verified with that parent present.`,
    { modal: true },
    "Rebase without parent",
  );
  if (ok !== "Rebase without parent") return;
  try {
    await client.request("changes.rebase", { id, apply: true });
  } catch (err) {
    void vscode.window.showErrorMessage(`Relo: could not rebase the change. ${errorText(err)}`);
  }
}

async function rejectWithDependents(id: string | undefined): Promise<void> {
  if (!id) return;
  const chg = state.changes.find((c) => c.id === id);
  const ok = await vscode.window.showWarningMessage(
    `Reject ${chg?.path ?? "this change"} and every change that builds on it? The agents' versions stay recoverable; your files are not touched.`,
    { modal: true },
    "Reject all",
  );
  if (ok !== "Reject all") return;
  try {
    await client.request("changes.reject", { id, withDependents: true });
  } catch (err) {
    void vscode.window.showErrorMessage(`Relo: could not reject the changes. ${errorText(err)}`);
  }
}

async function resolveChange(choice: "agent" | "mine" | "head", id: string | undefined): Promise<void> {
  if (!id) return;
  const chg = state.changes.find((c) => c.id === id);
  if (choice === "agent") {
    const picked = await gateDirty(chg?.path ? [chg.path] : [], "taking the agent's version");
    if (picked !== "Save") return;
    const ok = await vscode.window.showWarningMessage(
      `Replace ${chg?.path ?? "this file"} with the agent's version? Your current bytes are kept in the undo journal.`,
      { modal: true },
      "Take agent's version",
    );
    if (ok !== "Take agent's version") return;
  }
  if (choice === "mine") {
    const ok = await vscode.window.showWarningMessage(
      `Keep your version of ${chg?.path ?? "this file"}? The agent's version stays recoverable.`,
      { modal: true },
      "Keep mine",
    );
    if (ok !== "Keep mine") return;
  }
  try {
    await client.request("changes.resolve", { id, choice });
  } catch (err) {
    void vscode.window.showErrorMessage(`Relo: could not resolve the change. ${errorText(err)}`);
  }
}

async function decideAll(kind: "accept" | "reject"): Promise<void> {
  const pendingChanges = state.changes.filter((c) => c.status === "proposed");
  const pending = pendingChanges.length;
  if (!pending) {
    void vscode.window.showInformationMessage("Relo: there are no changes waiting for a decision.");
    return;
  }
  if (kind === "reject") {
    const choice = await vscode.window.showWarningMessage(`Reject all ${pending} change${pending === 1 ? "" : "s"}? The agents' versions are discarded; your files are not touched.`, { modal: true }, "Reject all");
    if (choice !== "Reject all") return;
  } else {
    const choice = await gateDirty(pendingChanges.map((c) => c.path), "accepting all changes");
    if (choice === "Cancel") return;
    // Skip leaves the dirty paths in the core's set, so accept-all lands the other paths and reports them.
  }
  const res = await client.request<{ done?: number; failed?: { path: string; reason: string }[] }>(kind === "accept" ? "changes.acceptAll" : "changes.rejectAll", {});
  const failed = res?.failed ?? [];
  if (failed.length) {
    void vscode.window.showWarningMessage(`Relo: ${res?.done ?? 0} done, ${failed.length} need attention. ${failed.slice(0, 3).map((f) => f.reason).join(" | ")}`);
  }
}

async function undoChange(id: string | undefined): Promise<void> {
  if (!id) return;
  const chg = state.changes.find((c) => c.id === id);
  const choice = await vscode.window.showWarningMessage(
    `Undo the accept of ${chg?.path ?? "this change"} and restore the bytes from before it?`,
    { modal: true },
    "Undo",
  );
  if (choice !== "Undo") return;
  try {
    await client.request("changes.undo", { id });
  } catch (err) {
    void vscode.window.showErrorMessage(`Relo: could not undo the change. ${errorText(err)}`);
  }
}

async function resolveConflicts(): Promise<void> {
  try {
    const res = await client.request<{ conflicted?: number; prompted?: boolean }>("changes.resolveConflicts", {});
    if (!res?.conflicted) {
      void vscode.window.showInformationMessage("Relo: no conflicts. Every proposed change merges cleanly.");
      return;
    }
    void vscode.window.showInformationMessage(`Relo: ${res.conflicted} conflicted change${res.conflicted === 1 ? "" : "s"} sent to the head agent.`);
  } catch (err) {
    void vscode.window.showErrorMessage(`Relo: could not resolve conflicts. ${errorText(err)}`);
  }
}

// Virtual documents for the two sides of a diff. The content comes from the core (changes.diff), keyed by URI.
const DIFF_SCHEME = "relo-diff";
const diffDocs = new Map<string, string>();
const diffProvider: vscode.TextDocumentContentProvider = {
  provideTextDocumentContent: (uri) => diffDocs.get(uri.toString()) ?? "",
};

function diffUri(side: "before" | "after", d: ChangeDiff): vscode.Uri {
  const uri = vscode.Uri.from({ scheme: DIFF_SCHEME, path: `/${side}/${d.path}`, query: d.id });
  diffDocs.set(uri.toString(), side === "before" ? d.before : d.after);
  if (diffDocs.size > 60) diffDocs.delete(diffDocs.keys().next().value as string);
  return uri;
}

async function openChangeDiff(id: string | undefined): Promise<void> {
  const chg = state.changes.find((c) => c.id === id);
  if (!chg) return;
  const d = await client.request<ChangeDiff>("changes.diff", { id: chg.id });
  if (d.binary) {
    void vscode.window.showInformationMessage(`Relo: ${d.path} is a binary file, so there is no text diff.`);
    return;
  }
  if (!d.hasBefore && !d.hasAfter) {
    if (chg.status === "applied") {
      await vscode.window.showTextDocument(vscode.Uri.file(path.join(workspacePath(), chg.path)));
      return;
    }
    void vscode.window.showInformationMessage(`Relo: ${d.note || "there is nothing to compare for " + d.path}`);
    return;
  }
  const title = `Relo: ${d.path}${d.kind ? ` (${d.kind})` : ""}${d.truncated ? " [truncated]" : ""}`;
  await vscode.commands.executeCommand("vscode.diff", diffUri("before", d), diffUri("after", d), title);
}

function updateStatus(): void {
  const running = state.agents.filter((a) => a.status === "running" || a.status === "starting").length;
  if (!state.cache) {
    status.tooltip = undefined;
    status.color = undefined;
  }
  if (!state.backend.ready) {
    status.text = state.backend.error ? "$(error) Relo: Failed" : "$(sync~spin) Relo: Starting";
    setCacheTooltip();
    return;
  }
  if (state.session?.status === "paused") {
    status.text = `${state.cache ? `$(database) ${Math.round(state.cache.session.hitRate * 100)}% cache · ` : ""}$(debug-pause) Relo: Paused`;
    setCacheTooltip();
    return;
  }
  if (running > 0) {
    status.text = `${state.cache ? `$(database) ${Math.round(state.cache.session.hitRate * 100)}% cache · ` : ""}$(sync~spin) Relo: ${running} agents`;
    setCacheTooltip();
    return;
  }
  status.text = `${state.cache ? `$(database) ${Math.round(state.cache.session.hitRate * 100)}% cache · ` : ""}$(hubot) Relo: Ready`;
  setCacheTooltip();
}

function setCacheTooltip(): void {
  if (!state.cache) return;
  const rate = state.cache.session.hitRate;
  status.color = new vscode.ThemeColor(rate >= 0.95 ? "charts.green" : rate >= 0.8 ? "charts.yellow" : "charts.red");
  status.tooltip = `Session cache hit rate: ${(rate * 100).toFixed(1)}%\n${state.cache.agents.map((a) => `${a.agentId}: ${(a.hitRate * 100).toFixed(1)}%`).join("\n")}`;
}

function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]!);
}

async function configureDifficultyRouting(): Promise<void> {
  const levels = ["trivial", "easy", "medium", "complex", "hard", "expert"];
  const config = vscode.workspace.getConfiguration("relo");
  const routing = { ...(config.get<Record<string, string>>("routing.byDifficulty") ?? {}) };
  const models = state.backend.models.map((m) => `${m.provider}/${m.id}`);
  for (const level of levels) {
    const picked = await vscode.window.showQuickPick(["(use tier default)", ...models], { title: `Model for ${level} tasks`, placeHolder: "Select model" });
    if (!picked) return;
    if (picked === "(use tier default)") delete routing[level];
    else routing[level] = picked;
  }
  await writeSetting("routing.byDifficulty", routing);
}

function cfg(): vscode.WorkspaceConfiguration {
  return vscode.workspace.getConfiguration("relo");
}
