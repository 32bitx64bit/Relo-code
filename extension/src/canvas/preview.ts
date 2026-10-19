import * as http from "node:http";
import { randomBytes } from "node:crypto";
import { compileCanvas, type CompiledCanvas } from "./runtime";
import type { CanvasStore } from "./store";
import type { CanvasDiagnostic } from "./types";

interface Preview { compiled: CompiledCanvas; revision: number; clients: Set<http.ServerResponse>; accessed: number; errors: CanvasDiagnostic[] }
function escape(value: string): string { return value.replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]!); }

export class CanvasPreviewServer {
  private server?: http.Server;
  private starting?: Promise<void>;
  private origin = "";
  private readonly secret = randomBytes(32).toString("hex");
  private readonly previews = new Map<string, Preview>();
  private readonly pending = new Map<string, CompiledCanvas>();
  private readonly timer: ReturnType<typeof setInterval>;
  private disposed = false;

  constructor(private readonly store: CanvasStore, private readonly options: { ttlMs?: number } = {}) {
    this.timer = setInterval(() => {
      for (const [id, preview] of this.previews) if (Date.now() - preview.accessed > (this.options.ttlMs ?? 30 * 60_000)) this.close(id);
    }, Math.min(options.ttlMs ?? 60_000, 60_000));
    this.timer.unref();
  }

  async url(id: string): Promise<string> {
    if (this.disposed) throw new Error("Canvas preview server is disposed");
    const document = await this.store.get(id);
    if (!this.previews.has(id)) this.previews.set(id, { compiled: this.pending.get(id) ?? await compileCanvas(document), revision: document.revision, clients: new Set(), accessed: Date.now(), errors: [] });
    await this.start();
    return `${this.origin}/${this.secret}/${id}/`;
  }

  publish(id: string, compiled: CompiledCanvas, revision?: number): void {
    this.pending.set(id, compiled);
    if (this.pending.size > 32) this.pending.delete(this.pending.keys().next().value!);
    const preview = this.previews.get(id);
    if (!preview) return;
    if (preview.compiled.html === compiled.html) return;
    preview.compiled = compiled;
    preview.revision = revision ?? preview.revision + 1;
    preview.errors = [];
    for (const client of preview.clients) {
      try {
        client.write(`data: ${preview.revision}\n\n`);
      } catch {
        preview.clients.delete(client);
        client.destroy();
      }
    }
  }

  diagnostics(id: string): CanvasDiagnostic[] { return this.previews.get(id)?.errors ?? []; }


  close(id: string): void {
    const preview = this.previews.get(id);
    if (preview) for (const client of preview.clients) client.end();
    this.previews.delete(id);
    this.pending.delete(id);
    if (!this.previews.size && this.server) {
      const server = this.server;
      this.server = undefined;
      this.origin = "";
      server.close();
      server.closeAllConnections();
    }
  }

  async dispose(): Promise<void> {
    this.disposed = true;
    clearInterval(this.timer);
    for (const id of [...this.previews.keys()]) this.close(id);
    this.pending.clear();
  }

  private async start(): Promise<void> {
    if (this.server) return;
    if (this.starting) return this.starting;
    this.starting = new Promise<void>((resolve, reject) => {
      const server = http.createServer((request, response) => { void this.serve(request, response).catch(() => { if (!response.headersSent) response.writeHead(500); response.end("Canvas preview failed"); }); });
      server.headersTimeout = 5000;
      server.requestTimeout = 10_000;
      server.maxHeadersCount = 32;
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => {
        const address = server.address();
        if (!address || typeof address === "string") { reject(new Error("No preview port")); return; }
        this.origin = `http://127.0.0.1:${address.port}`;
        this.server = server;
        resolve();
      });
    }).finally(() => { this.starting = undefined; });
    return this.starting;
  }

  private async serve(request: http.IncomingMessage, response: http.ServerResponse): Promise<void> {
    response.setHeader("X-Content-Type-Options", "nosniff");
    response.setHeader("Referrer-Policy", "no-referrer");
    response.setHeader("Cache-Control", "no-store");
    response.setHeader("Cross-Origin-Resource-Policy", "same-origin");
    if (request.headers.host !== new URL(this.origin).host || (request.headers.origin && request.headers.origin !== this.origin) || ["cross-site", "same-site"].includes(String(request.headers["sec-fetch-site"] ?? ""))) { response.writeHead(403).end(); return; }
    const match = new URL(request.url ?? "/", this.origin).pathname.match(new RegExp(`^/${this.secret}/([a-z0-9][a-z0-9-]{0,79})/(events|content|diagnostics)?$`));
    const preview = match && this.previews.get(match[1]);
    if (!match || !preview) { response.writeHead(404).end("Preview closed or not found"); return; }
    preview.accessed = Date.now();
    const endpoint = match[2] ?? "";
    if (request.method === "POST" && endpoint === "diagnostics") {
      let body = "";
      for await (const chunk of request) { body += String(chunk); if (body.length > 8192) { response.writeHead(413).end(); return; } }
      const raw: unknown = JSON.parse(body);
      if (Array.isArray(raw)) preview.errors = raw.slice(0, 20).flatMap((item: unknown) => {
        const value = item as { message?: unknown } | null;
        return typeof value?.message === "string" ? [{ code: "runtime", message: value.message.slice(0, 2000) }] : [];
      });
      response.writeHead(204).end();
      return;
    }
    if (request.method !== "GET") { response.writeHead(405).end(); return; }
    if (endpoint === "events") {
      response.writeHead(200, { "Content-Type": "text/event-stream", Connection: "keep-alive" });
      response.write(": connected\n\n");
      preview.clients.add(response);
      request.on("close", () => preview.clients.delete(response));
      response.on("error", () => preview.clients.delete(response));
      return;
    }
    if (endpoint === "content") { response.writeHead(200, { "Content-Type": "application/json" }); response.end(JSON.stringify({ html: preview.compiled.html, revision: preview.revision })); return; }
    if (endpoint) { response.writeHead(404).end(); return; }
    const nonce = randomBytes(24).toString("hex");
    const content = preview.compiled.html.replaceAll("__RELO_NONCE__", nonce);
    const csp = `default-src 'none'; script-src 'nonce-${nonce}'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; frame-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`;
    response.writeHead(200, { "Content-Type": "text/html; charset=utf-8", "Content-Security-Policy": csp });
    response.end(`<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><style>html,body,iframe{width:100%;height:100%;border:0;margin:0;display:block}body{overflow:hidden}</style><title>Relo Canvas</title></head><body><iframe sandbox="allow-scripts" title="Canvas preview" srcdoc="${escape(content)}"></iframe><script nonce="${nonce}">(()=>{const frame=document.querySelector('iframe');window.__canvasReady=false;window.addEventListener('message',event=>{if(event.source!==frame.contentWindow)return;const data=event.data;if(data?.type==='canvas.ready')window.__canvasReady=true;if(data?.type==='canvas.runtime'&&Array.isArray(data.errors))fetch('diagnostics',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(data.errors.slice(0,20))}).catch(()=>{});});const events=new EventSource('events');events.onmessage=async()=>{try{const response=await fetch('content');if(!response.ok)return;const content=await response.json();window.__canvasReady=false;frame.srcdoc=content.html.replaceAll('__RELO_NONCE__','${nonce}');}catch{}};window.addEventListener('pagehide',()=>events.close());})();</script></body></html>`);
  }
}
