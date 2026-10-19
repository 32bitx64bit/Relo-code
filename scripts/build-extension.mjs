import { cp, mkdir, readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

// VSIX packaging excludes root node_modules. Keep only the Canvas runtime's production dependency closure.
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../extension");
const require = createRequire(path.join(root, "package.json"));
const visited = new Set();
async function copyDependency(name) {
  if (visited.has(name)) return;
  visited.add(name);
  let source = path.dirname(require.resolve(name));
  let manifest;
  while (true) {
    try {
      const candidate = JSON.parse(await readFile(path.join(source, "package.json"), "utf8"));
      if (candidate.name === name) { manifest = candidate; break; }
    } catch { /* package export maps can hide package.json; walk from its resolved entry */ }
    const parent = path.dirname(source);
    if (source === parent) throw new Error(`Cannot locate runtime package ${name}`);
    source = parent;
  }
  const destination = path.join(root, "out/node_modules", name);
  await mkdir(path.dirname(destination), { recursive: true });
  await cp(source, destination, { recursive: true });
  for (const dependency of Object.keys(manifest.dependencies ?? {})) await copyDependency(dependency);
}
for (const name of ["esbuild-wasm", "react", "react-dom", "playwright-core", "parse5"]) await copyDependency(name);
