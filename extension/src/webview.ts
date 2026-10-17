import * as vscode from "vscode";
import { randomBytes } from "node:crypto";
import type { CoreClient } from "./client";
import type { ReloState } from "./protocol";

const POST_INTERVAL_MS = 50;

export class ChatView implements vscode.WebviewViewProvider {
  public static readonly viewType = "relo.chat";
  private view?: vscode.WebviewView;
  private postTimer?: ReturnType<typeof setTimeout>;

  constructor(
    private readonly ctx: vscode.ExtensionContext,
    private readonly client: CoreClient,
    private readonly getState: () => ReloState,
    private readonly onExtra: (m: unknown) => void,
  ) {}

  resolveWebviewView(webviewView: vscode.WebviewView): void {
    this.view = webviewView;
    webviewView.webview.options = { enableScripts: true, localResourceRoots: [this.ctx.extensionUri] };
    webviewView.webview.html = html(webviewView.webview, this.ctx, "chat");
    webviewView.webview.onDidReceiveMessage((m) => this.onMsg(m));
    this.post();
  }

  // Trailing-edge coalescing: at most one state post per POST_INTERVAL_MS; the flush reads the latest state.
  post(): void {
    if (this.postTimer) return;
    this.postTimer = setTimeout(() => {
      this.postTimer = undefined;
      this.view?.webview.postMessage({ type: "state", state: this.getState() });
    }, POST_INTERVAL_MS);
  }

  dispose(): void {
    if (this.postTimer) clearTimeout(this.postTimer);
    this.postTimer = undefined;
  }

  // Sends a non-state message (drawer open requests, settings replies) to the view.
  send(message: unknown): void {
    this.view?.webview.postMessage(message);
  }

  reveal(): void {
    this.view?.show(true);
  }

  private onMsg(m: { type: string; text?: string; mode?: string }): void {
    if (m.type === "submit" && m.text) {
      this.client.request("prompt.submit", { text: m.text }).catch((err: unknown) => {
        void vscode.window.showErrorMessage(`Relo: prompt failed: ${err instanceof Error ? err.message : String(err)}`);
      });
    }
    else if (m.type === "cancel") void this.client.request("prompt.cancel", {}).catch(() => {});
    else if (m.type === "mode" && m.mode) void this.client.request("mode.set", { mode: m.mode }).catch(() => {});
    else this.onExtra(m);
  }
}

export async function openDashboard(ctx: vscode.ExtensionContext, client: CoreClient, getState: () => ReloState, onMsg: (m: unknown) => void): Promise<vscode.WebviewPanel> {
  const panel = vscode.window.createWebviewPanel("relo.dashboard", "Relo Dashboard", vscode.ViewColumn.One, {
    enableScripts: true,
    retainContextWhenHidden: true,
    localResourceRoots: [ctx.extensionUri],
  });
  panel.webview.html = html(panel.webview, ctx, "dashboard");
  panel.webview.onDidReceiveMessage(onMsg);
  panel.webview.postMessage({ type: "state", state: getState() });
  return panel;
}

export async function openInspector(ctx: vscode.ExtensionContext, inspection: unknown): Promise<void> {
  const panel = vscode.window.createWebviewPanel("relo.context", "Relo Context", vscode.ViewColumn.Beside, { enableScripts: true, retainContextWhenHidden: true, localResourceRoots: [ctx.extensionUri] });
  panel.webview.html = html(panel.webview, ctx, "context");
  panel.webview.postMessage({ type: "state", state: { inspection } });
}

export function html(webview: vscode.Webview, ctx: vscode.ExtensionContext, view: string): string {
  const script = webview.asWebviewUri(vscode.Uri.joinPath(ctx.extensionUri, "media", "webview.js"));
  const codicons = webview.asWebviewUri(vscode.Uri.joinPath(ctx.extensionUri, "media", "codicon.css"));
  const nonce = randomBytes(16).toString("base64");
  return `<!doctype html>
<html>
<head>
<meta charset="UTF-8"/>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src ${webview.cspSource} data:; style-src ${webview.cspSource} 'unsafe-inline'; font-src ${webview.cspSource}; script-src 'nonce-${nonce}';"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Relo</title>
<link rel="stylesheet" href="${codicons}" />
<style>
:root { color-scheme: light dark; }
html,body{height:100%;margin:0;background:var(--vscode-editor-background);color:var(--vscode-foreground);font-family:var(--vscode-font-family,"Segoe UI",system-ui,sans-serif);font-size:var(--vscode-font-size,13px);line-height:1.4;}
*{box-sizing:border-box;}
button,textarea,select,input{font:inherit;color:inherit;}
button{background:var(--vscode-button-background);color:var(--vscode-button-foreground);border:none;padding:2px 8px;cursor:pointer;border-radius:2px;}
button:hover{background:var(--vscode-button-hoverBackground,var(--vscode-button-background));}
button.secondary{background:var(--vscode-button-secondaryBackground);color:var(--vscode-button-secondaryForeground);}
button.secondary:hover{background:var(--vscode-button-secondaryHoverBackground,var(--vscode-button-secondaryBackground));}
button:focus-visible,textarea:focus-visible,select:focus-visible,input:focus-visible,[role="menuitem"]:focus-visible,[role="treeitem"]:focus-visible{outline:1px solid var(--vscode-focusBorder);outline-offset:-1px;}
input,textarea,select{background:var(--vscode-input-background);border:1px solid var(--vscode-input-border,var(--vscode-panel-border));padding:2px 4px;border-radius:2px;}
.sr-only{position:absolute;width:1px;height:1px;margin:-1px;padding:0;overflow:hidden;clip:rect(0,0,0,0);border:0;}
#app{display:flex;flex-direction:column;height:100%;position:relative;}
.header{display:flex;align-items:center;gap:6px;min-height:24px;padding:2px 4px;border-bottom:1px solid var(--vscode-panel-border);flex-wrap:nowrap;min-width:0;}
.status-dot{width:8px;height:8px;border-radius:50%;background:var(--vscode-charts-green);flex:none;}
.status-dot.bad{background:var(--vscode-charts-red);}
.status-dot.busy{background:var(--vscode-charts-yellow);}
.header-title{font-weight:600;white-space:nowrap;flex:none;}
.header-session{flex:0 1 auto;min-width:0;overflow:hidden;text-overflow:ellipsis;}
.link-btn{background:transparent;color:var(--vscode-foreground);padding:0;text-align:left;border-radius:0;}
.link-btn:hover{background:transparent;text-decoration:underline;}
.header-meta{color:var(--vscode-descriptionForeground);font-size:11px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;min-width:0;}
.header-spacer{flex:1;}
@media (max-width: 320px){.header-meta{display:none;}}
.header-counts{flex:0 1 auto;}
.mode-select{font-size:12px;}
.messages{flex:1;min-height:0;overflow:auto;padding:6px 4px;display:flex;flex-direction:column;gap:10px;}
.empty{color:var(--vscode-descriptionForeground);padding:8px 2px;}
.msg{padding:0;}
.msg.agent{display:flex;flex-direction:column;gap:2px;padding:6px 8px;border-left:2px solid var(--vscode-charts-blue,var(--vscode-focusBorder));background:var(--vscode-editor-inactiveSelectionBackground,var(--vscode-textBlockQuote-background,transparent));border-radius:2px;}
.msg-role{font-size:11px;color:var(--vscode-descriptionForeground);margin-bottom:2px;display:flex;align-items:baseline;gap:4px;}
.msg.agent .msg-role{font-size:12px;font-weight:600;color:var(--vscode-foreground);margin-bottom:0;}
.msg.agent .msg-role .codicon{color:var(--vscode-charts-blue,var(--vscode-focusBorder));}
.msg-who{flex:none;}
.msg-queued{margin-left:auto;font-size:10px;font-weight:400;color:var(--vscode-descriptionForeground);border:1px solid var(--vscode-panel-border);border-radius:8px;padding:0 6px;}
.msg-text{word-wrap:break-word;}
.msg-text>:first-child{margin-top:0;}
.msg-text>:last-child{margin-bottom:0;}
.msg-text p{margin:0 0 .6em;}
.msg-text h1,.msg-text h2,.msg-text h3,.msg-text h4,.msg-text h5,.msg-text h6{margin:.8em 0 .35em;font-weight:600;line-height:1.3;}
.msg-text h1{font-size:1.25em;}
.msg-text h2{font-size:1.15em;}
.msg-text h3{font-size:1.05em;}
.msg-text h4,.msg-text h5,.msg-text h6{font-size:1em;}
.msg-text ul,.msg-text ol{margin:.2em 0 .6em;padding-left:1.4em;}
.msg-text li{margin:.15em 0;}
.msg-text li>ul,.msg-text li>ol{margin:.15em 0;}
.msg-text code{font-family:var(--vscode-editor-font-family,monospace);font-size:.92em;background:var(--vscode-textCodeBlock-background);padding:0 .3em;border-radius:3px;}
.msg-text .code-block code{background:none;padding:0;font-size:inherit;}
.msg-text a{color:var(--vscode-textLink-foreground);}
.msg-text a:hover{color:var(--vscode-textLink-activeForeground,var(--vscode-textLink-foreground));}
.msg-text blockquote{margin:.4em 0 .6em;padding:0 .8em;border-left:3px solid var(--vscode-textBlockQuote-border,var(--vscode-panel-border));color:var(--vscode-descriptionForeground);}
.msg-text hr{border:none;border-top:1px solid var(--vscode-panel-border);margin:.8em 0;}
.md-table{overflow-x:auto;max-width:100%;margin:.4em 0 .7em;}
.md-table table{border-collapse:collapse;font-size:.95em;}
.md-table th,.md-table td{border:1px solid var(--vscode-panel-border);padding:3px 8px;vertical-align:top;text-align:left;}
.md-table th{background:var(--vscode-editor-inactiveSelectionBackground,var(--vscode-textCodeBlock-background));font-weight:600;}
.md-table .al-c{text-align:center;}
.md-table .al-r{text-align:right;}
.msg.user .msg-text{background:var(--vscode-input-background);padding:4px 6px;border-radius:2px;display:inline-block;max-width:100%;}
.code-block{margin:4px 0;background:var(--vscode-textCodeBlock-background);border:1px solid var(--vscode-panel-border);border-radius:2px;overflow:hidden;}
.code-head{display:flex;justify-content:space-between;align-items:center;padding:2px 8px;font-size:11px;color:var(--vscode-descriptionForeground);border-bottom:1px solid var(--vscode-panel-border);}
.code-copy{background:transparent;border:none;padding:0 4px;color:var(--vscode-foreground);opacity:.6;cursor:pointer;font-size:13px;line-height:1;}
.code-copy:hover,.code-copy:focus-visible{opacity:1;background:transparent;}
.code-block pre{margin:0;padding:8px;overflow:auto;font-family:var(--vscode-editor-font-family,monospace);font-size:var(--vscode-editor-font-size,12px);}
.code-block code{font-family:inherit;}
.composer{border-top:1px solid var(--vscode-panel-border);padding:4px;display:flex;flex-direction:column;gap:4px;flex:none;}
.composer textarea{width:100%;min-height:28px;max-height:160px;resize:none;padding:4px 6px;border:1px solid var(--vscode-input-border,var(--vscode-panel-border));background:var(--vscode-input-background);border-radius:2px;outline:none;}
.toolbar{display:flex;align-items:center;gap:6px;font-size:12px;}
.toolbar-left{display:flex;align-items:center;gap:6px;min-width:0;flex:1;}
.toolbar-right{display:flex;align-items:center;gap:4px;}
.model-label{display:inline-flex;align-items:center;gap:2px;background:transparent;color:var(--vscode-descriptionForeground);padding:1px 4px;font-size:12px;min-width:0;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.model-label:hover{color:var(--vscode-foreground);background:var(--vscode-list-hoverBackground);}
.model-label .codicon{font-size:12px;flex:none;}
.text-link{background:transparent;border:none;padding:0;color:var(--vscode-textLink-foreground);cursor:pointer;text-align:left;}
.text-link:hover{background:transparent;text-decoration:underline;}
.msg.system .msg-text.sys{display:flex;gap:6px;align-items:flex-start;color:var(--vscode-descriptionForeground);font-size:12px;}
.msg.system .codicon{flex:none;margin-top:2px;}
.mode-select{min-width:0;max-width:100%;}
.send{background:var(--vscode-button-background);color:var(--vscode-button-foreground);border:none;padding:2px 8px;border-radius:2px;min-width:28px;}
.send:hover{background:var(--vscode-button-hoverBackground,var(--vscode-button-background));}
.stream{display:inline-block;width:6px;height:12px;background:var(--vscode-charts-blue);vertical-align:-2px;margin-left:3px;animation:stream-blink 1s steps(2,start) infinite;}
@keyframes stream-blink{to{visibility:hidden;}}
.msg.error .msg-text.err{display:flex;gap:6px;align-items:flex-start;color:var(--vscode-errorForeground);border:1px solid var(--vscode-inputValidation-errorBorder,var(--vscode-errorForeground));background:var(--vscode-inputValidation-errorBackground,transparent);border-radius:2px;padding:4px 6px;overflow-wrap:anywhere;font-size:12px;}
.msg.error .codicon{flex:none;margin-top:2px;}
.run{flex:none;border-top:1px solid var(--vscode-panel-border);padding:4px 6px;display:flex;flex-direction:column;gap:3px;min-width:0;font-size:12px;}
.run-status{display:flex;align-items:center;gap:6px;min-width:0;}
.run-status > .run-spin{color:var(--vscode-charts-blue);flex:none;}
.run-dot{display:none;width:8px;height:8px;border-radius:50%;background:var(--vscode-charts-blue);flex:none;}
@media (prefers-reduced-motion: reduce){.run-status > .run-spin{display:none;}.run-dot{display:inline-block;}.stream{animation:none;}.codicon-modifier-spin{animation:none !important;}}
.run-label{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-weight:600;}
.run-time{font-variant-numeric:tabular-nums;color:var(--vscode-descriptionForeground);flex:none;}
.run-stop{display:inline-flex;align-items:center;gap:3px;flex:none;padding:1px 6px;font-size:11px;background:var(--vscode-button-secondaryBackground);color:var(--vscode-button-secondaryForeground);}
.run-stop:hover{background:var(--vscode-button-secondaryHoverBackground,var(--vscode-button-secondaryBackground));}
.run-list-toggle{display:inline-flex;align-items:center;gap:2px;align-self:flex-start;background:transparent;color:var(--vscode-descriptionForeground);padding:0 2px;font-size:11px;text-transform:uppercase;font-weight:600;}
.run-list-toggle:hover{background:var(--vscode-list-hoverBackground);color:var(--vscode-foreground);}
.run-cards{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:3px;max-height:min(36vh,220px);overflow-y:auto;overscroll-behavior:contain;}
.run-card{border:1px solid var(--vscode-panel-border);border-radius:2px;min-width:0;}
.run-card-head{display:grid;grid-template-columns:auto minmax(0,1fr);grid-template-rows:auto auto;column-gap:6px;align-items:start;width:100%;text-align:left;background:transparent;color:var(--vscode-foreground);padding:4px 6px;border-radius:2px;}
.run-card-head:hover{background:var(--vscode-list-hoverBackground);}
.run-card-head .run-ico{grid-row:1 / 3;margin-top:1px;flex:none;}
.run-card-main{display:flex;flex-direction:column;min-width:0;}
.run-card-top{display:flex;align-items:baseline;gap:4px;min-width:0;}
.run-card-title{font-weight:500;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0;}
.run-role{font-size:10px;text-transform:uppercase;color:var(--vscode-descriptionForeground);flex:none;}
.run-card-live{color:var(--vscode-descriptionForeground);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:11px;}
.run-card-live.run-err{color:var(--vscode-errorForeground);white-space:normal;overflow-wrap:anywhere;}
.run-card-meta{grid-column:2;display:flex;gap:6px;flex-wrap:wrap;font-size:11px;color:var(--vscode-descriptionForeground);font-variant-numeric:tabular-nums;}
.run-card-body{padding:2px 6px 4px 22px;font-size:11px;color:var(--vscode-descriptionForeground);overflow-wrap:anywhere;}
.run-card-body[hidden]{display:none;}
.run-card.live .run-ico{color:var(--vscode-charts-blue);}
.run-card.wait .run-ico{color:var(--vscode-charts-yellow);}
.run-card.wait{border-color:var(--vscode-charts-yellow);}
.run-card.done{opacity:.75;}
.run-card.done .run-ico{color:var(--vscode-charts-green);}
.run-card.fail .run-ico{color:var(--vscode-charts-red);}
.run-card.fail{border-color:var(--vscode-charts-red);}
.run-card.stop .run-ico{color:var(--vscode-descriptionForeground);}
.run-card.queued .run-ico{color:var(--vscode-descriptionForeground);}
.run-card.queued{opacity:.85;}
.run-more{align-self:flex-start;background:transparent;color:var(--vscode-textLink-foreground);padding:0 2px;font-size:11px;}
.run-more:hover{background:transparent;text-decoration:underline;}
.cache-footer{border-top:1px solid var(--vscode-panel-border);padding:2px 6px;font-size:11px;color:var(--vscode-descriptionForeground);display:flex;flex-wrap:wrap;column-gap:8px;cursor:default;}
.section-header{display:flex;align-items:center;gap:2px;cursor:pointer;text-transform:uppercase;font-size:11px;font-weight:600;color:var(--vscode-sideBarSectionHeader-foreground,var(--vscode-foreground));padding:4px 0 2px;}
.section-header:hover{background:var(--vscode-list-hoverBackground);}
.section-body{padding:0 0 6px;}
.section-body[hidden]{display:none;}
.eff-table{width:100%;border-collapse:collapse;font-size:12px;margin:2px 0 4px;}
.eff-table th{text-align:left;font-weight:normal;color:var(--vscode-descriptionForeground);padding:2px 4px 2px 0;width:50%;word-break:break-word;}
.eff-table td{padding:2px 0;word-break:break-word;}
.agent-row{display:grid;grid-template-columns:auto 1fr;gap:6px;min-height:22px;padding:2px 4px;border-radius:2px;font-size:13px;}
.agent-row:hover{background:var(--vscode-list-hoverBackground);}
.agent-row.active{background:var(--vscode-list-activeSelectionBackground);color:var(--vscode-list-activeSelectionForeground);}
.agent-main{display:flex;flex-direction:column;gap:1px;min-width:0;}
.agent-top{display:flex;gap:4px;align-items:baseline;}
.agent-name{font-weight:500;}
.agent-role,.agent-model{color:var(--vscode-descriptionForeground);font-size:11px;}
.agent-escalated{color:var(--vscode-descriptionForeground);font-size:11px;}
.agent-track{height:2px;background:var(--vscode-list-inactiveSelectionBackground,var(--vscode-panel-border));margin-top:2px;}
.agent-bar{height:2px;background:var(--vscode-progressBar-background);width:0;}
.agent-children{margin-left:16px;border-left:1px solid var(--vscode-panel-border);padding-left:8px;}
.task-row{min-height:22px;padding:2px 4px;font-size:13px;display:flex;gap:4px;align-items:baseline;flex-wrap:wrap;}
.task-meta{color:var(--vscode-descriptionForeground);font-size:11px;}
.inspector{padding:6px 4px;}
.bar-seg{height:6px;display:flex;width:100%;border:1px solid var(--vscode-panel-border);border-radius:2px;overflow:hidden;}
.bar-seg span{display:block;height:100%;}
.meta{color:var(--vscode-descriptionForeground);font-size:11px;}
ul{margin:4px 0;padding-left:16px;}
li{margin:2px 0;font-size:13px;}
.cache-line{font-size:12px;color:var(--vscode-descriptionForeground);padding:4px;}
.dashboard-grid{display:grid;grid-template-columns:minmax(180px,1fr) minmax(280px,2fr) minmax(160px,1fr);grid-template-rows:1fr auto;height:calc(100% - 24px);}
.col{border-right:1px solid var(--vscode-panel-border);overflow:auto;padding:6px 4px;}
.bar{grid-column:1/-1;display:flex;gap:12px;padding:4px 8px;border-top:1px solid var(--vscode-panel-border);font-size:12px;color:var(--vscode-descriptionForeground);}
@media (max-width: 720px){.dashboard-grid{grid-template-columns:1fr;}}
/* condensed main view */
.icon-btn{background:transparent;color:var(--vscode-foreground);padding:2px 4px;min-width:22px;height:22px;display:inline-flex;align-items:center;justify-content:center;}
.icon-btn:hover{background:var(--vscode-toolbar-hoverBackground);}
.icon-btn[aria-expanded="true"]{background:var(--vscode-toolbar-activeBackground,var(--vscode-toolbar-hoverBackground));}
.menu{position:absolute;top:26px;right:4px;min-width:200px;background:var(--vscode-menu-background);color:var(--vscode-menu-foreground);border:1px solid var(--vscode-menu-border,var(--vscode-panel-border));box-shadow:0 2px 8px var(--vscode-widget-shadow,transparent);padding:4px 0;z-index:10;}
.menu[hidden]{display:none;}
.menu [role="menuitem"]{display:flex;align-items:center;gap:6px;width:100%;padding:3px 10px;background:transparent;border:none;text-align:left;border-radius:0;font-size:12px;}
.menu [role="menuitem"]:hover,.menu [role="menuitem"]:focus{background:var(--vscode-menu-selectionBackground);color:var(--vscode-menu-selectionForeground);outline:none;}
.menu [role="separator"]{height:1px;background:var(--vscode-menu-separatorBackground,var(--vscode-panel-border));margin:4px 0;}
.strip{display:flex;align-items:center;gap:2px;height:24px;min-height:24px;padding:0 4px;flex-wrap:nowrap;flex:none;overflow-x:auto;overflow-x:clip;scrollbar-width:none;}
.strip::-webkit-scrollbar{display:none;}
.chip{display:inline-flex;align-items:center;gap:3px;height:22px;background:transparent;color:var(--vscode-descriptionForeground);padding:2px 6px;font-size:11px;border:none;border-bottom:2px solid transparent;border-radius:0;white-space:nowrap;flex:none;}
.chip:hover{color:var(--vscode-foreground);background:var(--vscode-toolbar-hoverBackground);}
.chip.open{color:var(--vscode-foreground);border-bottom-color:var(--vscode-focusBorder);}
.chip.dim{opacity:.55;}
.chip-value{font-variant-numeric:tabular-nums;}
@media (max-width: 300px){.strip{overflow-x:auto;}}
@media (min-width: 301px){.strip{overflow-x:hidden;}}
.ctx-bar{height:2px;background:var(--vscode-panel-border);flex:none;}
.ctx-bar span{display:block;height:100%;background:var(--vscode-progressBar-background);width:0;}
.drawer{flex:none;max-height:40%;overflow:auto;border-bottom:1px solid var(--vscode-panel-border);padding:2px 4px 4px;}
.drawer[hidden]{display:none;}
.drawer-row{display:flex;align-items:center;gap:6px;min-height:22px;padding:1px 4px;font-size:13px;border-radius:2px;}
.drawer-row:hover{background:var(--vscode-list-hoverBackground);}
.drawer-row .grow{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
.drawer-row .desc{color:var(--vscode-descriptionForeground);font-size:11px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;}
.drawer-row .actions{display:none;gap:0;}
.drawer-row:hover .actions,.drawer-row:focus-within .actions,.drawer-row .actions.shown{display:inline-flex;}
.icon-btn:disabled{opacity:.4;cursor:default;background:transparent;}
.badge{background:var(--vscode-badge-background);color:var(--vscode-badge-foreground);font-size:10px;padding:0 4px;border-radius:8px;line-height:14px;flex:none;}
.letter{font-family:var(--vscode-editor-font-family);font-size:11px;width:12px;text-align:center;flex:none;}
.letter.M{color:var(--vscode-gitDecoration-modifiedResourceForeground);}
.letter.A{color:var(--vscode-gitDecoration-addedResourceForeground);}
.letter.D{color:var(--vscode-gitDecoration-deletedResourceForeground);}
.mini{font-size:11px;color:var(--vscode-descriptionForeground);}
.check-out{margin:2px 0 4px 22px;white-space:pre-wrap;font-family:var(--vscode-editor-font-family);font-size:11px;color:var(--vscode-descriptionForeground);max-height:120px;overflow:auto;}
.drawer-foot{display:flex;gap:6px;padding:2px 0;}
.composer-foot{display:flex;align-items:center;gap:6px;}
.eff-link{background:transparent;color:var(--vscode-descriptionForeground);padding:0 4px;font-size:11px;}
.eff-link:hover{color:var(--vscode-foreground);background:transparent;}
/* settings page */
.settings{flex:1;min-height:0;overflow:auto;display:flex;flex-direction:column;}
.settings-head{display:flex;align-items:center;gap:4px;padding:4px;position:sticky;top:0;background:var(--vscode-editor-background);z-index:2;border-bottom:1px solid var(--vscode-panel-border);}
.settings-head input{flex:1;min-width:0;}
.settings-group{padding:0 4px;}
.settings-group > h3{margin:8px 0 2px;font-size:11px;font-weight:600;text-transform:uppercase;color:var(--vscode-sideBarSectionHeader-foreground,var(--vscode-foreground));cursor:pointer;display:flex;align-items:center;gap:2px;}
.settings-group > h3 .codicon{font-size:12px;}
.setting{position:relative;padding:4px 6px 6px 8px;border-bottom:1px solid var(--vscode-panel-border);display:grid;grid-template-columns:1fr auto;column-gap:6px;}
.setting.modified::before{content:"";position:absolute;left:0;top:0;bottom:0;width:2px;background:var(--vscode-settings-modifiedItemIndicator,var(--vscode-progressBar-background));}
.setting-label{font-size:12px;font-weight:500;min-width:0;overflow-wrap:anywhere;}
.setting-desc{grid-column:1 / 2;font-size:11px;color:var(--vscode-descriptionForeground);overflow-wrap:anywhere;}
.setting-ctl{grid-row:1 / 3;grid-column:2;display:flex;align-items:flex-start;gap:2px;max-width:150px;}
.setting-ctl select,.setting-ctl input[type="text"],.setting-ctl input[type="number"]{width:100%;min-width:0;font-size:12px;}
.setting-ctl input[type="checkbox"]{margin:2px 0;}
.setting-reset{background:transparent;padding:0 2px;min-width:18px;height:18px;opacity:.7;}
.setting-reset:hover{background:transparent;opacity:1;}
.settings-group[hidden],.setting[hidden]{display:none;}
.settings-empty{padding:8px 4px;color:var(--vscode-descriptionForeground);}
.settings-foot{padding:8px 4px;display:flex;flex-direction:column;gap:6px;align-items:flex-start;}
.settings-foot a{color:var(--vscode-textLink-foreground);}
.settings-foot a:focus-visible{outline:1px solid var(--vscode-focusBorder);}
.table-line{display:flex;gap:6px;align-items:center;}
.table-line select{flex:1;min-width:0;}
.table-line:has(.mpick.open){align-items:flex-start;}
.table-line:has(.mpick.open) .lvl{padding-top:4px;}
.lvl{width:48px;flex:none;}
.setting.wide{grid-template-columns:1fr;}
.setting.wide .setting-ctl{grid-row:auto;grid-column:1;max-width:none;flex-direction:column;align-items:stretch;gap:3px;margin-top:3px;}
.setting.wide .setting-ctl select{width:100%;}
.setting.wide .setting-reset{position:absolute;top:2px;right:2px;}
.model-detail{margin-top:1px;}
.mpick{position:relative;min-width:0;flex:1;}
.mpick-btn{width:100%;display:flex;align-items:center;justify-content:space-between;gap:4px;text-align:left;background:var(--vscode-input-background);color:var(--vscode-input-foreground,var(--vscode-foreground));border:1px solid var(--vscode-input-border,var(--vscode-panel-border));border-radius:2px;padding:2px 4px;font-size:12px;min-width:0;}
.mpick-btn:hover{background:var(--vscode-input-background);border-color:var(--vscode-focusBorder);}
.mpick-val{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0;}
.mpick-q{width:100%;font-size:12px;}
.mpick-list{list-style:none;margin:2px 0 0;padding:0;max-height:220px;overflow:auto;border:1px solid var(--vscode-focusBorder);border-radius:2px;background:var(--vscode-editor-background);}
.mpick-list li{display:flex;align-items:center;gap:4px;padding:2px 6px;font-size:12px;cursor:pointer;min-width:0;}
.mpick-list li:hover{background:var(--vscode-list-hoverBackground);}
.mpick-list li.active{background:var(--vscode-list-activeSelectionBackground);color:var(--vscode-list-activeSelectionForeground);}
.mp-group{font-weight:600;color:var(--vscode-descriptionForeground);}
.mp-group.static{cursor:default;}
.mp-group .codicon{font-size:12px;flex:none;}
.mp-prov{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0;}
.mp-opt{padding-left:20px;}
.mp-opt.mp-blank{padding-left:6px;}
.mp-id{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0;flex:0 1 auto;}
.mp-hint{margin-left:auto;white-space:nowrap;flex:none;padding-left:6px;}
.mp-check{flex:none;}
.mp-opt.active .mp-hint{color:inherit;opacity:.8;}
.mpick-count{min-height:14px;padding:1px 2px;}
.effort-line{display:flex;align-items:center;gap:6px;}
.effort-line select{flex:1;min-width:0;}
.seg{display:flex;border:1px solid var(--vscode-input-border,var(--vscode-panel-border));border-radius:2px;overflow:hidden;}
.seg button{flex:1;background:transparent;color:var(--vscode-foreground);border-radius:0;padding:2px 8px;font-size:12px;}
.seg button + button{border-left:1px solid var(--vscode-input-border,var(--vscode-panel-border));}
.seg button:hover{background:var(--vscode-toolbar-hoverBackground);}
.seg button[aria-checked="true"]{background:var(--vscode-button-background);color:var(--vscode-button-foreground);}
.prov{grid-template-columns:1fr auto;}
.prov-head{display:flex;align-items:center;gap:6px;min-width:0;}
.prov-name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0;}
.prov-url{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;grid-column:1;}
.prov-meta{grid-column:1;}
.prov-actions{grid-row:1 / 4;grid-column:2;display:flex;align-items:center;gap:2px;}
.key-ok{color:var(--vscode-gitDecoration-addedResourceForeground,var(--vscode-foreground));}
.key-missing{color:var(--vscode-descriptionForeground);}
.prov-empty{padding:6px 8px;}
.pf-fields{display:flex;flex-direction:column;gap:5px;margin-top:4px;}
.pf-field{display:flex;flex-direction:column;gap:1px;}
.pf-field input,.pf-field select{width:100%;min-width:0;font-size:12px;}
.pf-actions{display:flex;align-items:center;gap:6px;}
.pf-error{color:var(--vscode-errorForeground);font-size:11px;overflow-wrap:anywhere;}
.pf-issue{color:var(--vscode-errorForeground);overflow-wrap:anywhere;}
.pf-models{display:flex;flex-direction:column;gap:4px;}
.pm-row{border:1px solid var(--vscode-panel-border);border-radius:2px;padding:3px;display:flex;flex-direction:column;gap:3px;}
.pm-top{display:flex;gap:2px;align-items:center;}
.pm-top input{flex:1;min-width:0;font-size:12px;}
.pm-grid{display:grid;grid-template-columns:1fr 1fr;gap:3px;}
.pm-grid input{min-width:0;width:100%;font-size:12px;}
.pm-grid input:last-child{grid-column:1 / 3;}
button:disabled{opacity:.5;cursor:default;}
.drawer-row.dim{opacity:.6;}
/* session history page */
.hist{flex:1;min-height:0;overflow:auto;display:flex;flex-direction:column;}
.hist-head{display:flex;align-items:center;justify-content:space-between;gap:6px;padding:6px 6px 4px;position:sticky;top:0;background:var(--vscode-editor-background);border-bottom:1px solid var(--vscode-panel-border);}
.hist-title{font-size:11px;font-weight:600;text-transform:uppercase;color:var(--vscode-descriptionForeground);}
.hist-list{list-style:none;margin:0;padding:2px 4px;}
.hist-row{display:flex;align-items:center;gap:4px;border-radius:2px;margin:1px 0;}
.hist-row:hover{background:var(--vscode-list-hoverBackground);}
.hist-row.current{background:var(--vscode-list-inactiveSelectionBackground);}
.hist-open{flex:1;min-width:0;display:flex;flex-direction:column;align-items:flex-start;gap:1px;background:transparent;color:var(--vscode-foreground);padding:4px 6px;text-align:left;border-radius:2px;}
.hist-open:hover{background:transparent;}
.hist-name{max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-weight:500;}
.hist-meta{font-size:11px;color:var(--vscode-descriptionForeground);}
.hist-empty{padding:12px 8px;color:var(--vscode-descriptionForeground);}
.table-line code{font-size:11px;}
@media (max-width: 300px){.chip{padding:1px 4px;}.setting-ctl{max-width:110px;}}
</style>
</head>
<body data-view="${view}">
<div id="app"><div class="empty">Loading Relo…</div></div>
<script nonce="${nonce}" src="${script}"></script>
</body>
</html>`;
}
