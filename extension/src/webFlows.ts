// vscode-bound flows for relo_web. The pure helpers in web.ts hold all the parsing and message logic; this file only
// calls vscode and the core around them.

import * as vscode from "vscode";
import { Methods } from "./protocol";
import { discoveryPrompt, discoveryUsable, parseDiscovery, parseStatus } from "./web";
import { challengeMessage, challengeResponse, checkWebChallenge } from "./webChallenge";

const MAX_URL = 8192;

type Core = { request<T>(method: string, params?: unknown): Promise<T> };

export type WebDeps = {
  client: Core;
  config: () => { get<T>(key: string, def?: T): T },
  // writeSetting in extension.ts clears a leftover workspace value; a workspace value would silently override the confirm.
  writeSetting: (key: string, value: unknown) => Promise<void>;
};

// Runs web.discover for a search URL and, if the user confirms, saves the template. The template is never written
// without a confirm; Cancel, dismissal or a failed discovery leaves the setting alone.
export async function discoverSearchProvider(deps: WebDeps, url: string): Promise<string | undefined> {
  const target = (url || "").trim();
  if (!target) {
    void vscode.window.showInformationMessage("Relo: set relo.web.customSearchUrl first, then run Re-detect search provider.");
    return undefined;
  }
  if (target.length > MAX_URL) {
    void vscode.window.showErrorMessage("Relo: the search URL is too long (limit 8192 characters).");
    return undefined;
  }
  const reply = await deps.client.request<unknown>(Methods.webDiscover, { url: target }).catch((err: unknown) => {
    void vscode.window.showErrorMessage(`Relo: search provider test failed: ${err instanceof Error ? err.message : String(err)}`);
    return undefined;
  });
  if (reply === undefined) return undefined;
  const d = parseDiscovery(reply);
  if (d.error) {
    void vscode.window.showErrorMessage(`Relo: search provider test failed: ${d.error}`);
    return undefined;
  }
  if (!discoveryUsable(d)) {
    void vscode.window.showErrorMessage("Relo: the search page gave no usable template; type a URL with %s where the query goes.");
    return undefined;
  }
  const choice = await vscode.window.showInformationMessage(discoveryPrompt(d), { modal: true }, "Use this template", "Cancel");
  if (choice !== "Use this template") return undefined;
  await deps.writeSetting("web.customSearchTemplate", d.template);
  void vscode.window.showInformationMessage(`Relo: custom search template saved for ${d.origin || "the custom provider"}.`);
  return d.template;
}

// The command entry point: tests the URL currently in settings, which is what the user just edited.
export function redetectSearchProvider(deps: WebDeps): Promise<string | undefined> {
  return discoverSearchProvider(deps, deps.config().get<string>("web.customSearchUrl", ""));
}

export async function showWebStatus(deps: WebDeps): Promise<void> {
  const raw = await deps.client.request<unknown>(Methods.webStatus, {});
  const s = parseStatus(raw);
  const panel = vscode.window.createWebviewPanel("relo.webStatus", "Relo: Web", vscode.ViewColumn.Active, { enableScripts: false });
  const cell = (label: string, value: string) => `<tr><th>${escapeHtml(label)}</th><td>${escapeHtml(value)}</td></tr>`;
  panel.webview.html = `<!doctype html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>body{font-family:var(--vscode-font-family);color:var(--vscode-foreground);padding:1rem}table{border-collapse:collapse}th,td{border:1px solid var(--vscode-panel-border);padding:.4rem;text-align:left}th{background:var(--vscode-editor-background)}pre{white-space:pre-wrap}</style></head><body><h2>Relo web</h2><table>${cell("Ready", s.ready ? "yes" : "no")}${cell("Mode", s.mode || "auto")}${cell("Search provider", s.provider || "unknown")}${cell("Browser", s.browserPath || "not found")}${cell("Open tabs", String(s.tabs))}${cell("Agents browsing", String(s.agents))}</table><p>Opening a browser is left to the engine. Run "Relo: Re-detect search provider" after editing the custom search URL.</p></body></html>`;
}

// A browser challenge is never auto-approved, not even in YOLO: the orchestrator leaves web- ids out of SetYolo, so this
// prompt is always answered here. Done answers once (a challenge is site state, not a standing rule); dismissal rejects.
export async function askWebChallenge(deps: WebDeps, params: unknown): Promise<void> {
  const c = checkWebChallenge(params);
  if (!c) return;
  const detail = c.url ? `\n${c.url}` : "";
  const choice = await vscode.window.showWarningMessage(`${challengeMessage(c)}${detail}`, { modal: true }, "Done", "Cancel");
  await deps.client.request("permission.respond", { id: c.id, response: challengeResponse(choice) }).catch(() => {});
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[ch] ?? ch);
}
