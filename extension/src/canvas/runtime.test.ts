import { test } from "node:test";
import * as assert from "node:assert/strict";
import { compileCanvas } from "./runtime";
import type { CanvasDocument } from "./types";

function artifact(format: CanvasDocument["format"], entry: string, files: Record<string, string>): CanvasDocument {
  return { version: 1, id: "inventory", name: "Inventory", format, entry, files, revision: 1, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() };
}

test("HTML Canvas bundles local JS/CSS and converts inline event handlers under nonce CSP", async () => {
  const compiled = await compileCanvas(artifact("html", "index.html", {
    "index.html": '<link rel="stylesheet" href="style.css"><button onclick="increment()">Equip</button><output id="count">0</output><script src="main.js"></script>',
    "main.js": "let count=0;function increment(){document.getElementById('count').textContent=String(++count)}",
    "style.css": "body{background:#17191d}",
  }));
  assert.deepEqual(compiled.errors, []);
  assert.match(compiled.html, /addEventListener\("click"/);
  assert.match(compiled.html, /background:#17191d/);
  assert.doesNotMatch(compiled.html, /onclick=/);
  assert.match(compiled.html, /connect-src 'none'/);
  assert.match(compiled.html, /nonce-__RELO_NONCE__/);
  assert.match(compiled.html, /script-src 'nonce-__RELO_NONCE__'/);
  assert.match(compiled.html, /frame-src 'none'/);
  assert.match(compiled.html, /object-src 'none'/);
  assert.match(compiled.html, /base-uri 'none'/);
  assert.match(compiled.html, /form-action 'none'/);
});

test("React Canvas transpiles TSX with local components and CSS", async () => {
  const compiled = await compileCanvas(artifact("react", "App.tsx", {
    "App.tsx": "import {useState} from 'react';import './style.css';export default function App(){const [n,setN]=useState<number>(0);return <button onClick={()=>setN(n+1)}>Equip {n}</button>}",
    "style.css": "button{color:gold}",
  }));
  assert.deepEqual(compiled.errors, []);
  assert.doesNotMatch(compiled.html, /useState<number>/);
  assert.match(compiled.html, /Equip/);
  assert.match(compiled.html, /color: gold/);
});

test("Canvas reports compilation errors and blocks host module imports", async () => {
  const compiled = await compileCanvas(artifact("react", "App.tsx", { "App.tsx": "import fs from 'node:fs';export default ()=> <p>{fs.readFileSync('/etc/passwd')}</p>" }));
  assert.ok(compiled.errors.some((error) => error.code === "compile" && /local files or React/.test(error.message)));
  assert.match(compiled.html, /role="alert"/);
});

test("Canvas SVG and embedded assets never require file access", async () => {
  const doc = artifact("svg", "drawing.svg", { "drawing.svg": '<svg xmlns="http://www.w3.org/2000/svg"><image href="icon.png"/><text>Inventory</text></svg>' });
  doc.assets = { "icon.png": { mime: "image/png", data: "aGVsbG8=" } };
  const compiled = await compileCanvas(doc);
  assert.deepEqual(compiled.errors, []);
  assert.match(compiled.html, /data:image\/png;base64,aGVsbG8=/);
});

test("Canvas removes nested browsing contexts and reports missing assets", async () => {
  const compiled = await compileCanvas(artifact("html", "index.html", { "index.html": '<iframe src="file:///etc/passwd"></iframe><img src="https://remote.example/track"><script>fetch("https://remote.example/")</script>' }));
  assert.ok(compiled.errors.some((error) => error.code === "asset"));
  assert.doesNotMatch(compiled.html, /<iframe/);
  assert.match(compiled.html, /connect-src 'none'/);
});

test("HTML module scripts bundle their own relative imports", async () => {
  const compiled = await compileCanvas(artifact("html", "index.html", {
    "index.html": '<output id="value"></output><script type="module" src="js/main.js"></script>',
    "js/main.js": "import {label} from './util.js';document.getElementById('value').textContent=label;",
    "js/util.js": "export const label='Module inventory';",
  }));
  assert.deepEqual(compiled.errors, []);
  assert.match(compiled.html, /Module inventory/);
});

test("React Canvas bundles embedded assets referenced from imported CSS", async () => {
  const document = artifact("react", "App.tsx", {
    "App.tsx": "import './style.css';export default ()=> <div>Inventory</div>",
    "style.css": "body{background-image:url('./stone.png')}",
  });
  document.assets = { "stone.png": { mime: "image/png", data: "aGVsbG8=" } };
  const compiled = await compileCanvas(document);
  assert.deepEqual(compiled.errors, []);
  assert.match(compiled.html, /data:image\/png[,;]/);
});
