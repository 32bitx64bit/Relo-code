import { build, type Loader, type Plugin } from "esbuild-wasm";
import { parse, serialize, type DefaultTreeAdapterMap } from "parse5";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import type { CanvasDocument, CanvasDiagnostic } from "./types";

type Node = DefaultTreeAdapterMap["node"];
type Element = DefaultTreeAdapterMap["element"];
export interface CompiledCanvas { html: string; errors: CanvasDiagnostic[] }
export const CANVAS_NONCE = "__RELO_NONCE__";

function text(value: string): string { return value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;"); }
function sourceScript(value: string): string { return value.replace(/<\/script/gi, "<\\/script"); }
function loader(file: string): Loader {
  const ext = path.posix.extname(file).slice(1);
  return ({ ts: "ts", tsx: "tsx", jsx: "jsx", js: "js", mjs: "js", css: "css", json: "json" } as Record<string, Loader>)[ext] ?? "text";
}

// All imports resolve from the artifact's virtual files, except these extension-owned React distributions.
async function bundle(document: CanvasDocument, entry: string, extra: Record<string, string> = {}): Promise<{ script: string; css: string }> {
  const files = { ...document.files, ...extra };
  const packageRoot = (name: string): string => path.dirname(require.resolve(`${name}/package.json`));
  const react = packageRoot("react"), dom = packageRoot("react-dom"), scheduler = packageRoot("scheduler");
  const packages = new Map([["react", react], ["react-dom", dom], ["scheduler", scheduler]]);
  const plugin: Plugin = {
    name: "canvas-virtual-files",
    setup(api) {
      api.onResolve({ filter: /.*/ }, (args) => {
        if (packages.has(args.path.split("/")[0])) {
          const pkg = args.path.split("/")[0];
          const subpath = args.path.slice(pkg.length + 1);
          if (subpath && !["client", "jsx-runtime", "jsx-dev-runtime"].includes(subpath)) throw new Error(`Unsupported Canvas package import: ${args.path}`);
          const resolved = require.resolve(args.path);
          return { path: resolved, namespace: "canvas-react" };
        }
        if (args.namespace === "canvas-react") {
          if (!args.path.startsWith(".")) throw new Error(`Unsupported React dependency ${args.path}`);
          const resolved = path.resolve(path.dirname(args.importer), args.path);
          if (![...packages.values()].some((root) => resolved.startsWith(root + path.sep))) throw new Error("Package import escapes its distribution");
          return { path: resolved, namespace: "canvas-react" };
        }
        const candidate = args.kind === "entry-point" ? args.path : path.posix.normalize(path.posix.join(path.posix.dirname(args.importer), args.path));
        if (args.kind !== "entry-point" && !args.path.startsWith(".")) throw new Error(`Canvas imports must be local files or React: ${args.path}`);
        if (candidate.startsWith("../") || path.posix.isAbsolute(candidate)) throw new Error("Canvas import escapes its artifact");
        const resolved = [candidate, ...[".tsx", ".ts", ".jsx", ".js", ".css", ".json", "/index.tsx", "/index.ts", "/index.js"].map((ext) => candidate + ext)].find((file) => Object.hasOwn(files, file));
        const asset = document.assets?.[candidate];
        if (asset) return args.kind === "url-token" ? { path: candidate, namespace: "canvas-asset-file" } : { path: `data:${asset.mime};base64,${asset.data}`, namespace: "canvas-asset" };
        if (!resolved) throw new Error(`Missing Canvas source: ${candidate}`);
        return { path: resolved, namespace: "canvas-source" };
      });
      api.onLoad({ filter: /.*/, namespace: "canvas-react" }, async (args) => ({ contents: await fs.readFile(args.path, "utf8"), loader: "js" }));
      api.onLoad({ filter: /.*/, namespace: "canvas-asset" }, (args) => ({ contents: `export default ${JSON.stringify(args.path)}`, loader: "js" }));
      api.onLoad({ filter: /\.(?:png|jpe?g|webp|gif|woff2?)$/i, namespace: "canvas-asset-file" }, (args) => ({ contents: Buffer.from(document.assets![args.path].data, "base64"), loader: "dataurl" }));
      api.onLoad({ filter: /.*/, namespace: "canvas-source" }, (args) => ({ contents: files[args.path], loader: loader(args.path) }));
    },
  };
  const result = await build({ entryPoints: [entry], bundle: true, write: false, outdir: "canvas-output", format: "iife", platform: "browser", target: "es2020", jsx: "automatic", define: { "process.env.NODE_ENV": '"production"' }, plugins: [plugin], logLevel: "silent" });
  return { script: result.outputFiles?.filter((file) => file.path.endsWith(".js")).map((file) => file.text).join("\n") ?? "", css: result.outputFiles?.filter((file) => file.path.endsWith(".css")).map((file) => file.text).join("\n") ?? "" };
}

function diagnostic(error: unknown): CanvasDiagnostic[] {
  const value = error as { errors?: Array<{ text: string; location?: { file: string; line: number; column: number } }> };
  return value.errors?.slice(0, 20).map((item) => ({ code: "compile", message: item.text, file: item.location?.file, line: item.location?.line, column: item.location?.column })) ?? [{ code: "compile", message: error instanceof Error ? error.message : String(error) }];
}

export async function compileCanvas(document: CanvasDocument): Promise<CompiledCanvas> {
  const errors: CanvasDiagnostic[] = [];
  let markup = "", script = "", css = "";
  const virtual: Record<string, string> = {};
  const assets = document.assets ?? {};
  let bodyAttributes = "", htmlAttributes = "";
  const resolveAsset = (ref: string, base = document.entry): string => {
    if (!ref || ref.startsWith("#") || /^data:(?:image\/(?:png|jpeg|gif|webp|svg\+xml)|font\/(?:woff2?|ttf|otf))[;,]/i.test(ref)) return ref;
    const key = path.posix.normalize(path.posix.join(path.posix.dirname(base), ref.replace(/^\.\//, "")));
    const asset = assets[key];
    if (!asset) {
      errors.push({ code: "asset", message: `Missing or blocked Canvas asset: ${ref}`, file: base });
      return "data:,";
    }
    return `data:${asset.mime};base64,${asset.data}`;
  };
  const rewriteCSS = (source: string, base = document.entry): string => source.replace(/url\(\s*(["']?)([^)'"\s]+)\1\s*\)/gi, (_match, _quote: string, ref: string) => `url("${resolveAsset(ref, base)}")`).replace(/@import\s+(?:url\([^)]*\)|["'][^"']*["'])\s*;?/gi, () => { errors.push({ code: "asset", message: "Use local bundled CSS imports, not CSS @import URLs" }); return ""; });
  try {
    if (document.format === "react") {
      const entry = "__canvas_mount.tsx";
      virtual[entry] = `import React from 'react';import{createRoot}from'react-dom/client';import App from './${document.entry}';createRoot(document.getElementById('root')).render(React.createElement(App));`;
      ({ script, css } = await bundle(document, entry, virtual));
      markup = '<div id="root"></div>';
    } else {
      const tree = parse(document.format === "svg" ? `<!doctype html><html><body>${document.files[document.entry]}</body></html>` : document.files[document.entry]);
      const scripts: string[] = [], handlers: string[] = [];
      let index = 0;
      const visit = (node: Node): void => {
        if (!("childNodes" in node)) return;
        for (const child of [...node.childNodes]) {
          if ("tagName" in child) {
            const element = child as Element;
            const attr = (name: string) => element.attrs.find((item) => item.name === name)?.value;
            const contents = () => element.childNodes.map((item) => "value" in item ? item.value : "").join("");
            if (element.tagName === "html" || element.tagName === "body") {
              const serialized = element.attrs.filter((item) => !/^on/i.test(item.name) && !["nonce", "srcdoc"].includes(item.name)).map((item) => ` ${item.name}="${text(item.name === "style" ? rewriteCSS(item.value) : item.value).replace(/"/g, "&quot;")}"`).join("");
              if (element.tagName === "html") htmlAttributes = serialized;
              else bodyAttributes = serialized;
            }
            if (element.tagName === "script") {
              const type = attr("type") ?? "";
              if (!["", "text/javascript", "application/javascript", "module"].includes(type)) {
                if (type !== "application/json") errors.push({ code: "compile", message: `Unsupported script type ${type}` });
              } else if (attr("src")) {
                const ref = attr("src")!;
                const file = path.posix.normalize(path.posix.join(path.posix.dirname(document.entry), ref));
                if (!Object.hasOwn(document.files, file)) throw new Error(`Missing or blocked script: ${ref}`);
                scripts.push(type === "module" ? `import './${file}';` : document.files[file]);
              } else if (type === "module") {
                const file = `__canvas_module_${index++}.js`;
                virtual[file] = contents();
                scripts.push(`import './${file}';`);
              } else {
                scripts.push(contents());
              }
              node.childNodes.splice(node.childNodes.indexOf(child), 1);
              continue;
            }
            if (element.tagName === "style") {
              css += "\n" + rewriteCSS(contents());
              node.childNodes.splice(node.childNodes.indexOf(child), 1);
              continue;
            }
            if (element.tagName === "link" && attr("rel") === "stylesheet") {
              const file = path.posix.normalize(path.posix.join(path.posix.dirname(document.entry), attr("href") ?? ""));
              if (!Object.hasOwn(document.files, file)) throw new Error(`Missing or blocked stylesheet: ${file}`);
              css += "\n" + rewriteCSS(document.files[file], file);
              node.childNodes.splice(node.childNodes.indexOf(child), 1);
              continue;
            }
            if (["iframe", "object", "embed", "base", "meta"].includes(element.tagName)) {
              node.childNodes.splice(node.childNodes.indexOf(child), 1);
              continue;
            }
            const identifier = `canvas-${index++}`;
            let hasHandler = false;
            element.attrs = element.attrs.filter((item) => {
              if (!/^on[a-z]+$/i.test(item.name)) return true;
              handlers.push(`document.querySelector('[data-relo-node="${identifier}"]').addEventListener(${JSON.stringify(item.name.slice(2))},function(event){const result=(function(event){${item.value}\n}).call(this,event);if(result===false)event.preventDefault();});`);
              hasHandler = true;
              return false;
            });
            if (hasHandler) element.attrs.push({ name: "data-relo-node", value: identifier });
            for (const item of element.attrs) {
              if (["src", "poster"].includes(item.name) || (element.tagName === "image" && ["href", "xlink:href"].includes(item.name))) item.value = resolveAsset(item.value);
              if (item.name === "style") item.value = rewriteCSS(item.value);
              if (["srcset", "formaction", "action", "target", "srcdoc", "nonce"].includes(item.name)) item.value = "";
              if (element.tagName !== "image" && item.name === "href" && !item.value.startsWith("#")) item.value = "#";
            }
          }
          visit(child);
        }
      };
      visit(tree);
      const root = tree.childNodes.find((node): node is Element => "tagName" in node && node.tagName === "html");
      markup = root ? root.childNodes.filter((node): node is Element => "tagName" in node && ["head", "body"].includes(node.tagName)).map((node) => serialize(node)).join("") : serialize(tree);
      if (scripts.length || handlers.length) {
        virtual["__canvas_entry.js"] = scripts.join("\n") + "\n" + handlers.join("\n");
        const bundled = await bundle(document, "__canvas_entry.js", virtual);
        script = bundled.script;
        css += bundled.css;
      }
    }
  } catch (error) { errors.push(...diagnostic(error)); }
  css = rewriteCSS(css);
  const observer = `(()=>{const report=message=>parent.postMessage({type:'canvas.runtime',errors:[{code:'runtime',message:String(message).slice(0,2000)}]},'*');window.addEventListener('error',event=>report(event.message));window.addEventListener('unhandledrejection',event=>report(event.reason?.message||event.reason));document.addEventListener('click',event=>{const anchor=event.target.closest?.('a');if(anchor&&!anchor.getAttribute('href')?.startsWith('#'))event.preventDefault();});document.addEventListener('submit',event=>event.preventDefault());window.addEventListener('load',()=>parent.postMessage({type:'canvas.ready'},'*'));})();`;
  const csp = `default-src 'none'; script-src 'nonce-${CANVAS_NONCE}'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; worker-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'`;
  const html = `<!doctype html><html${htmlAttributes}><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="Content-Security-Policy" content="${csp}"><style>html,body{margin:0;min-height:100%;}*{box-sizing:border-box}${css.replace(/<\/style/gi, "<\\/style")}</style><script nonce="${CANVAS_NONCE}">${sourceScript(observer)}</script></head><body${bodyAttributes}>${errors.some((error) => error.code === "compile") ? `<pre role="alert">${text(errors.map((error) => error.message).join("\n"))}</pre>` : markup}<script nonce="${CANVAS_NONCE}">${sourceScript(script)}</script></body></html>`;
  return { html, errors };
}
