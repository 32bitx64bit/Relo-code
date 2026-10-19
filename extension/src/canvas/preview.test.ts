import { test } from "node:test";
import * as assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import * as http from "node:http";
import { chromium } from "playwright-core";
import { CanvasStore } from "./store";
import { CanvasPreviewServer } from "./preview";
import { CanvasCaptureService } from "./capture";
import { compileCanvas } from "./runtime";

test("preview is on-demand, secret-scoped, synchronized, and disposable", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "relo-canvas-preview-"));
  const store = new CanvasStore(() => root);
  const preview = new CanvasPreviewServer(store);
  try {
    const doc = await store.create({ action: "create", name: "Inventory", format: "html", entry: "index.html", files: { "index.html": "<h1>Inventory</h1>" } });
    const url = await preview.url(doc.id);
    const response = await fetch(url);
    assert.equal(response.status, 200);
    assert.match(response.headers.get("content-security-policy")!, /frame-ancestors 'none'/);
    assert.match(await response.text(), /sandbox="allow-scripts"/);
    assert.equal((await fetch(new URL("/", url))).status, 404);
    const forgedHost = await new Promise<number>((resolve) => {
      const request = http.get(url, { headers: { Host: "evil.example" } }, (result) => { result.resume(); resolve(result.statusCode!); });
      request.on("error", () => resolve(0));
    });
    assert.equal(forgedHost, 403);
    const crossSite = await fetch(url, { headers: { "Sec-Fetch-Site": "cross-site" } });
    assert.equal(crossSite.status, 403);
    const updates = await fetch(new URL("events", url));
    const reader = updates.body!.getReader();
    await reader.read();
    preview.publish(doc.id, { html: "<h1>Dark stone</h1>", errors: [] }, 2);
    const update = new TextDecoder().decode((await reader.read()).value);
    assert.match(update, /data: 2/);
    const changed = await (await fetch(new URL("content", url))).json() as { html: string };
    assert.match(changed.html, /Dark stone/);
    await reader.cancel();
    preview.close(doc.id);
    await assert.rejects(fetch(url));
    assert.equal((await store.get(doc.id)).id, doc.id);
  } finally { await preview.dispose(); await rm(root, { recursive: true, force: true }); }
});

test("real browser renders interactive HTML and React, hot reloads, and captures", { skip: !process.env.RELO_E2E_CANVAS }, async () => {
  const root = await mkdtemp(path.join(tmpdir(), "relo-canvas-browser-"));
  const store = new CanvasStore(() => root);
  const preview = new CanvasPreviewServer(store);
  const browser = await chromium.launch({ executablePath: process.env.RELO_CANVAS_BROWSER ?? "/usr/bin/chromium", headless: true, chromiumSandbox: process.env.RELO_CANVAS_NO_SANDBOX !== "1" });
  try {
    const doc = await store.create({ action: "create", name: "Inventory", format: "html", entry: "index.html", files: { "index.html": '<button onclick="this.textContent=\'Equipped\'">Equip</button><script>window.hostAPI=typeof acquireVsCodeApi;try{parent.document.body.dataset.escaped="yes"}catch{}</script>' } });
    const url = await preview.url(doc.id);
    const page = await browser.newPage();
    await page.goto(url);
    const frame = page.frameLocator("iframe");
    await frame.getByRole("button", { name: "Equip", exact: true }).click();
    await assert.doesNotReject(frame.getByRole("button", { name: "Equipped" }).waitFor());
    assert.equal(await page.evaluate("document.body.dataset.escaped"), undefined);
    const child = page.frames().find((item) => item.parentFrame())!;
    assert.equal(await child.evaluate("window.hostAPI"), "undefined");
    const react = await store.update({ action: "update", id: doc.id, revision: doc.revision, format: "react", entry: "App.tsx", files: { "App.tsx": "import{useState}from'react';export default function App(){const[n,setN]=useState<number>(0);return <button onClick={()=>setN(n+1)}>Dark stone {n}</button>}" } });
    preview.publish(doc.id, await compileCanvas(react), react.revision);
    await frame.getByRole("button", { name: "Dark stone 0" }).click();
    await frame.getByRole("button", { name: "Dark stone 1" }).waitFor();
    const capture = await new CanvasCaptureService(preview).capture(doc.id, { width: 800, height: 600 });
    assert.equal(capture.png.subarray(1, 4).toString(), "PNG");
    assert.equal(capture.visualInspectionAvailable, false);
    assert.deepEqual(capture.errors, []);
    preview.publish(doc.id, await compileCanvas({ ...react, format: "html", entry: "index.html", files: { "index.html": "<script>throw new Error('canvas-broken')</script>" } }), 3);
    const broken = await new CanvasCaptureService(preview).capture(doc.id, { width: 640, height: 480 });
    assert.ok(broken.errors.some((error) => /canvas-broken/.test(error.message)), "runtime errors must be inspectable");
    await page.close();
  } finally { await browser.close(); await preview.dispose(); await rm(root, { recursive: true, force: true }); }
});
