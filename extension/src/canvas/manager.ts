import * as vscode from "vscode";
import * as path from "node:path";
import { CanvasStore } from "./store";
import { compileCanvas } from "./runtime";
import { CanvasPreviewServer } from "./preview";
import { CanvasCaptureService } from "./capture";
import { parseCanvasDocument, parseCanvasRequest, type CanvasDiagnostic, type CanvasDocument, type CanvasRequest } from "./types";
import { CANVAS_EDITOR, CanvasEditorProvider, canvasEditorHTML } from "./editor";

export interface CanvasBridgeRequest {
  requestId: string;
  sessionId: string;
  agentId: string;
  workspace: string;
  request: unknown;
}

export class CanvasManager implements vscode.Disposable {
  readonly store: CanvasStore;
  readonly editor: CanvasEditorProvider;
  private readonly preview: CanvasPreviewServer;
  private readonly capture: CanvasCaptureService;
  private readonly errors = new Map<string, CanvasDiagnostic[]>();
  private readonly runtimeErrors = new Map<string, CanvasDiagnostic[]>();
  private readonly generations = new WeakMap<vscode.WebviewPanel, number>();
  private readonly rendered = new Set<string>();
  private readonly subscriptions: vscode.Disposable[];
  private activeID?: string;
  private disposed = false;

  constructor(private readonly root: () => string) {
    const workspace = root();
    this.root = () => workspace;
    this.store = new CanvasStore(this.root);
    this.preview = new CanvasPreviewServer(this.store);
    this.capture = new CanvasCaptureService(this.preview);
    this.editor = new CanvasEditorProvider({
      render: (document, panel) => this.render(document, panel),
      action: async (action, document) => {
        if (action === "source") {
          await vscode.commands.executeCommand("vscode.openWith", document.uri, "default");
          return;
        }
        const artifact = await this.document(document);
        if (action === "refresh") await this.editor.refresh(document);
        else if (action === "capture") await this.saveCapture(artifact.id);
        else await this.execute({ action, id: artifact.id });
      },
      diagnostics: (document, errors) => {
        try { this.runtimeErrors.set(parseCanvasDocument(JSON.parse(document.getText())).id, errors); } catch { /* invalid source is shown by render */ }
      },
      changed: async (document) => {
        try { await this.publish(await this.document(document)); } catch { /* render reports malformed source */ }
      },
      ready: (document) => {
        try { this.rendered.add(parseCanvasDocument(JSON.parse(document.getText())).id); } catch { /* invalid source */ }
      },
    });
    this.subscriptions = [
      vscode.window.registerCustomEditorProvider(CANVAS_EDITOR, this.editor, { supportsMultipleEditorsPerDocument: true }),
      vscode.workspace.onDidSaveTextDocument((document) => {
        if (document.uri.fsPath.endsWith(".relo-canvas")) void this.refreshSaved(document).catch(() => {});
      }),
    ];
    const watcher = vscode.workspace.createFileSystemWatcher(new vscode.RelativePattern(workspace, ".relo/canvas/*.relo-canvas"));
    watcher.onDidChange((uri) => { void this.changed(uri); });
    watcher.onDidDelete((uri) => {
      const id = path.basename(uri.fsPath, ".relo-canvas");
      this.preview.close(id);
      this.editor.close(uri);
    });
    this.subscriptions.push(watcher);
  }

  async handle(raw: unknown, sessionID?: string): Promise<unknown> {
    if (!raw || typeof raw !== "object") throw new Error("Invalid Canvas request");
    const event = raw as CanvasBridgeRequest;
    if (event.sessionId !== sessionID || path.resolve(event.workspace) !== path.resolve(this.root())) throw new Error("Canvas request belongs to a different session or repository");
    return this.execute(event.request);
  }

  async execute(raw: unknown): Promise<unknown> {
    if (this.disposed) throw new Error("Canvas is closed");
    const request = parseCanvasRequest(raw);
    if (request.action === "list") return { ok: true, artifacts: (await this.store.list()).map((artifact) => ({ id: artifact.id, name: artifact.name, format: artifact.format, entry: artifact.entry, revision: artifact.revision, createdAt: artifact.createdAt, updatedAt: artifact.updatedAt, files: Object.keys(artifact.files), assets: Object.keys(artifact.assets ?? {}) })) };
    if (!vscode.workspace.isTrusted) throw new Error("Canvas rendering requires a trusted workspace");
    if (request.action === "create") {
      const artifact = await this.store.create(request);
      await this.publish(artifact);
      await this.open(artifact.id);
      return this.info(artifact);
    }
    const id = request.id ?? this.activeID;
    if (!id) throw new Error("Specify a Canvas artifact ID or open a Canvas first");
    let artifact = await this.store.get(id);
    if (["inspect", "capture", "popout"].includes(request.action)) {
      const document = vscode.workspace.textDocuments.find((item) => item.uri.fsPath === this.store.artifactPath(id));
      if (document?.isDirty) artifact = await this.document(document);
    }
    if (request.action === "update") {
      const uri = vscode.Uri.file(this.store.artifactPath(id));
      if (vscode.workspace.textDocuments.some((document) => document.uri.toString() === uri.toString() && document.isDirty)) {
        throw new Error("Canvas source has unsaved edits; save it before an agent update");
      }
      artifact = await this.store.update({ ...request, id });
      await this.publish(artifact);
      await this.changed(uri);
      return this.info(artifact);
    }
    if (request.action === "open") await this.open(id);
    if (request.action === "popout") {
      const source = await vscode.workspace.openTextDocument(vscode.Uri.file(this.store.artifactPath(id)));
      this.editor.track(source);
      const url = await this.preview.url(id);
      await this.publish(artifact);
      if (!await vscode.env.openExternal(vscode.Uri.parse(url))) throw new Error("Could not open the Canvas browser preview");
      return { ...this.info(artifact), preview: { kind: "external-browser", live: true } };
    }
      if (request.action === "close") {
      this.editor.close(vscode.Uri.file(this.store.artifactPath(id)));
      this.preview.close(id);
      return { ...this.info(artifact), closed: true, persisted: true };
    }
    if (request.action === "capture") return this.screenshot(artifact, request);
    if (request.action === "inspect") {
      await this.publish(artifact);
      return { ...this.info(artifact), source: { files: artifact.files, assets: Object.entries(artifact.assets ?? {}).map(([name, asset]) => ({ name, mime: asset.mime })) }, errors: [...this.errors.get(id) ?? [], ...this.runtimeErrors.get(id) ?? [], ...this.preview.diagnostics(id)], capture: { available: true, action: "capture", visualInspectionAvailable: false }, verification: "No screenshot has been inspected by a model. Capture returns image data for a vision-capable caller." };
    }
    return this.info(artifact);
  }

  async pickAndOpen(): Promise<void> {
    const artifacts = await this.store.list();
    const selection = await vscode.window.showQuickPick(artifacts.map((artifact) => ({ label: artifact.name, description: artifact.id, id: artifact.id })), { placeHolder: "Open a saved Canvas" });
    if (selection) await this.execute({ action: "open", id: selection.id });
  }

  async saveCapture(id?: string): Promise<void> {
    const result = await this.execute({ action: "capture", ...(id ? { id } : {}) }) as { artifact?: { id: string }; capture?: { data?: string }; error?: { message: string } };
    if (!result.capture?.data) throw new Error(result.error?.message ?? "Canvas capture was unavailable");
    const destination = await vscode.window.showSaveDialog({ title: "Save Canvas Capture", filters: { "PNG Image": ["png"] }, defaultUri: vscode.Uri.file(path.join(this.root(), `${result.artifact?.id ?? "canvas"}.png`)) });
    if (destination) await vscode.workspace.fs.writeFile(destination, Buffer.from(result.capture.data, "base64"));
  }


  private info(artifact: CanvasDocument): Record<string, unknown> {
    return { ok: true, artifact: { id: artifact.id, name: artifact.name, format: artifact.format, entry: artifact.entry, revision: artifact.revision, createdAt: artifact.createdAt, updatedAt: artifact.updatedAt, source: this.store.artifactPath(artifact.id), files: Object.keys(artifact.files), assets: Object.keys(artifact.assets ?? {}) }, views: this.editor.count(vscode.Uri.file(this.store.artifactPath(artifact.id))), rendered: this.rendered.has(artifact.id), errors: this.errors.get(artifact.id) ?? [] };
  }

  private async open(id: string): Promise<void> {
    this.activeID = id;
    const uri = vscode.Uri.file(this.store.artifactPath(id));
    if (this.editor.reveal(uri)) return;
    await vscode.commands.executeCommand("vscode.openWith", uri, CANVAS_EDITOR, { preview: false, viewColumn: vscode.ViewColumn.Active });
    if (this.editor.count(uri) > 0) await this.waitRendered(id, 3000);
  }

  private async waitRendered(id: string, timeoutMs: number): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (!this.rendered.has(id) && !this.disposed && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 50));
  }

  private async document(document: vscode.TextDocument): Promise<CanvasDocument> {
    const artifact = parseCanvasDocument(JSON.parse(document.getText()));
    if (path.resolve(document.uri.fsPath) !== path.resolve(this.store.artifactPath(artifact.id))) throw new Error("Canvas must be inside the selected repository's .relo/canvas directory, with its ID as filename");
    await this.store.get(artifact.id); // validates the persisted path and rejects symlink escapes
    return artifact;
  }

  private async publish(artifact: CanvasDocument): Promise<{ html: string; errors: CanvasDiagnostic[] }> {
    const compiled = await compileCanvas(artifact);
    this.errors.set(artifact.id, compiled.errors);
    this.preview.publish(artifact.id, compiled, artifact.revision);
    return compiled;
  }

  private async render(document: vscode.TextDocument, panel: vscode.WebviewPanel): Promise<void> {
    const generation = (this.generations.get(panel) ?? 0) + 1;
    this.generations.set(panel, generation);
    try {
      if (!vscode.workspace.isTrusted) throw new Error("Trust this workspace to render interactive Canvas content");
      const artifact = await this.document(document);
      this.rendered.delete(artifact.id);
      this.runtimeErrors.delete(artifact.id);
      const compiled = await this.publish(artifact);
      if (this.generations.get(panel) !== generation || this.disposed) return;
      panel.title = artifact.name;
      panel.webview.html = canvasEditorHTML(artifact, compiled.html, compiled.errors);
      this.activeID = artifact.id;
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      panel.webview.html = canvasEditorHTML({ name: "Canvas source error" }, "", [{ code: "source", message }]);
    }
  }

  private async changed(uri: vscode.Uri): Promise<void> {
    const document = vscode.workspace.textDocuments.find((item) => item.uri.toString() === uri.toString());
    if (document) await this.editor.refresh(document);
    else {
      try { await this.publish(await this.store.get(path.basename(uri.fsPath, ".relo-canvas"))); } catch { /* errors surface on inspect or open */ }
    }
  }

  private async refreshSaved(document: vscode.TextDocument): Promise<void> { await this.publish(await this.document(document)); }

  private async screenshot(artifact: CanvasDocument, request: CanvasRequest): Promise<unknown> {
    await this.publish(artifact);
    try {
      const capture = await this.capture.capture(artifact.id, { width: request.width, height: request.height });
      return { ...this.info(artifact), capture: { mime: "image/png", data: capture.pngBase64, width: capture.width, height: capture.height, errors: capture.errors, visualInspectionAvailable: capture.visualInspectionAvailable, inspection: capture.inspection }, verification: "Screenshot captured; visual correctness requires image-capable model inspection." };
    } catch (error) {
      return { ...this.info(artifact), ok: false, capture: { available: false, visualInspectionAvailable: false }, error: { code: "capture_unavailable", message: error instanceof Error ? error.message : String(error) } };
    }
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.editor.dispose();
    for (const subscription of this.subscriptions) subscription.dispose();
    void this.preview.dispose();
  }
}
