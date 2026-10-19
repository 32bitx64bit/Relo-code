import * as vscode from "vscode";
import { randomBytes } from "node:crypto";
import type { CanvasDiagnostic, CanvasDocument } from "./types";

export const CANVAS_EDITOR = "relo.canvas";

export interface CanvasEditorHost {
  render(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void>;
  action(action: "refresh" | "popout" | "source" | "capture", document: vscode.TextDocument): Promise<void>;
  diagnostics(document: vscode.TextDocument, errors: CanvasDiagnostic[]): void;
  changed(document: vscode.TextDocument): Promise<void>;
  ready(document: vscode.TextDocument): void;
}

export class CanvasEditorProvider implements vscode.CustomTextEditorProvider, vscode.Disposable {
  private readonly views = new Map<string, Set<vscode.WebviewPanel>>();
  private readonly disposables: vscode.Disposable[] = [];
  private readonly timers = new Map<string, ReturnType<typeof setTimeout>>();

  constructor(private readonly host: CanvasEditorHost) {
    this.disposables.push(vscode.workspace.onDidChangeTextDocument((event) => {
      const key = event.document.uri.toString();
      if (!this.views.has(key)) return;
      clearTimeout(this.timers.get(key));
      this.timers.set(key, setTimeout(() => {
        this.timers.delete(key);
        void this.host.changed(event.document);
        void this.refresh(event.document);
      }, 180));
    }));
  }

  track(document: vscode.TextDocument): void {
    const key = document.uri.toString();
    if (!this.views.has(key)) this.views.set(key, new Set());
  }

  async resolveCustomTextEditor(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void> {
    const key = document.uri.toString();
    const views = this.views.get(key) ?? new Set<vscode.WebviewPanel>();
    views.add(panel);
    this.views.set(key, views);
    panel.webview.options = { enableScripts: true, localResourceRoots: [] };
    const messages = panel.webview.onDidReceiveMessage((raw: unknown) => {
      if (!raw || typeof raw !== "object") return;
      const message = raw as Record<string, unknown>;
      if (message.type === "ready") this.host.ready(document);
      else if (message.type === "action" && ["refresh", "popout", "source", "capture"].includes(String(message.action))) {
        void this.host.action(message.action as "refresh" | "popout" | "source" | "capture", document)
          .catch((error: unknown) => vscode.window.showErrorMessage(`Relo Canvas: ${error instanceof Error ? error.message : String(error)}`));
      } else if (message.type === "diagnostics" && Array.isArray(message.errors)) {
        const errors = message.errors.slice(0, 20).flatMap((value: unknown): CanvasDiagnostic[] => {
          if (!value || typeof value !== "object") return [];
          const item = value as Record<string, unknown>;
          if (typeof item.message !== "string") return [];
          return [{ code: "runtime", message: item.message.slice(0, 2000) }];
        });
        this.host.diagnostics(document, errors);
      }
    });
    panel.onDidDispose(() => {
      messages.dispose();
      views.delete(panel);
      if (!views.size) this.views.delete(key);
    });
    await this.host.render(document, panel);
  }

  async refresh(document: vscode.TextDocument): Promise<void> {
    await Promise.all([...this.views.get(document.uri.toString()) ?? []].map((panel) => this.host.render(document, panel)));
  }

  count(uri: vscode.Uri): number { return this.views.get(uri.toString())?.size ?? 0; }

  reveal(uri: vscode.Uri): boolean {
    const panel = this.views.get(uri.toString())?.values().next().value;
    if (!panel) return false;
    panel.reveal(vscode.ViewColumn.Active, false);
    return true;
  }

  close(uri: vscode.Uri): void {
    for (const panel of this.views.get(uri.toString()) ?? []) panel.dispose();
  }

  dispose(): void {
    for (const timer of this.timers.values()) clearTimeout(timer);
    for (const views of this.views.values()) for (const panel of views) panel.dispose();
    for (const item of this.disposables) item.dispose();
    this.views.clear();
  }
}

function escape(value: string): string {
  return value.replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]!);
}

// The shell owns the VS Code API. Artifact code runs only in an opaque-origin sandbox without this API.
export function canvasEditorHTML(artifact: Pick<CanvasDocument, "name">, html: string, errors: CanvasDiagnostic[]): string {
  const nonce = randomBytes(24).toString("hex");
  const frame = html.replaceAll("__RELO_NONCE__", nonce).replace(/<script\b[^>]*>/gi, (tag) => tag.replace(/\snonce\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)/gi, "").replace(/>$/, ` nonce="${nonce}">`));
  const errorText = errors.map((error) => error.message).join("\n");
  return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; frame-src 'self'; connect-src 'none'; worker-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'">
<style>html,body{height:100%;margin:0;overflow:hidden;background:var(--vscode-editor-background);color:var(--vscode-foreground);font:13px var(--vscode-font-family)}body{display:flex;flex-direction:column}header{display:flex;align-items:center;gap:8px;padding:7px 10px;border-bottom:1px solid var(--vscode-panel-border)}strong{margin-right:auto;font-weight:500}button{background:var(--vscode-button-secondaryBackground);color:var(--vscode-button-secondaryForeground);border:0;padding:4px 8px;cursor:pointer}button:focus-visible{outline:1px solid var(--vscode-focusBorder)}iframe{width:100%;flex:1;border:0;background:white}pre{white-space:pre-wrap;margin:0;padding:12px;color:var(--vscode-errorForeground)}[hidden]{display:none}</style></head><body>
<header><strong>${escape(artifact.name)}</strong><button data-action="refresh">Refresh</button><button data-action="popout">Pop Out</button><button data-action="capture">Capture</button><button data-action="source">Source</button></header>
<pre id="errors" ${errorText ? "" : "hidden"}>${escape(errorText)}</pre><iframe title="${escape(artifact.name)}" sandbox="allow-scripts" srcdoc="${escape(frame)}"></iframe>
<script nonce="${nonce}">(()=>{const api=acquireVsCodeApi();const frame=document.querySelector('iframe');for(const button of document.querySelectorAll('button'))button.addEventListener('click',()=>api.postMessage({type:'action',action:button.dataset.action}));window.addEventListener('message',event=>{if(event.source!==frame.contentWindow)return;const data=event.data;if(data?.type==='canvas.ready'){api.postMessage({type:'ready'});return;}if(!data||data.type!=='canvas.runtime')return;const errors=Array.isArray(data.errors)?data.errors:typeof data.message==='string'?[{message:data.message}]:[];api.postMessage({type:'diagnostics',errors:errors.slice(0,20)});});})();</script></body></html>`;
}
