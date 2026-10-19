import * as assert from "node:assert/strict";
import * as vscode from "vscode";
import { CanvasStore } from "./store";

// Invoked only by VS Code's --extensionTestsPath in an isolated test window.
export async function run(): Promise<void> {
  const root = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  assert.ok(root, "Smoke test needs an isolated workspace");
  const extension = vscode.extensions.getExtension("relocode.relo-code");
  assert.ok(extension);
  await extension.activate();
  const store = new CanvasStore(() => root);
  const artifact = await store.create({ action: "create", name: "Canvas Native Smoke", format: "html", entry: "index.html", files: { "index.html": '<button onclick="this.textContent=\'Ready\'">Inventory</button>' } });
  const uri = vscode.Uri.file(store.artifactPath(artifact.id));
  const count = () => vscode.window.tabGroups.all.flatMap((group) => group.tabs).filter((tab) => tab.input instanceof vscode.TabInputCustom && tab.input.viewType === "relo.canvas").length;
  assert.equal(count(), 0, "Canvas must not open at startup");
  await vscode.commands.executeCommand("vscode.openWith", uri, "relo.canvas", { preview: false });
  assert.equal(count(), 1, "Canvas must open in an ordinary custom editor tab");
  let rendered = false;
  for (let attempt = 0; attempt < 40; attempt++) {
    const info = await vscode.commands.executeCommand<{ rendered: boolean; errors: unknown[] }>("relo.canvas.inspect");
    if (info?.rendered) { rendered = true; assert.deepEqual(info.errors, []); break; }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.ok(rendered, "Native Canvas iframe must execute and report render readiness through the isolated shell");
  await vscode.commands.executeCommand("workbench.action.splitEditorRight");
  assert.equal(count(), 2, "Canvas must support native editor splits");
  const updated = await store.update({ action: "update", id: artifact.id, revision: 1, files: { "index.html": "<h1>Dark Stone Inventory</h1>" } });
  assert.equal(updated.revision, 2);
  await vscode.commands.executeCommand("workbench.action.closeAllEditors");
  assert.equal(count(), 0);
  assert.equal((await store.get(artifact.id)).revision, 2, "Closing the editor must not delete the artifact");
  await vscode.commands.executeCommand("vscode.openWith", uri, "relo.canvas", { preview: false });
  assert.equal(count(), 1, "Saved Canvas must reopen");
  await vscode.commands.executeCommand("workbench.action.closeAllEditors");
}
