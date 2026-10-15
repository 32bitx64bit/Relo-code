import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const root = resolve(fileURLToPath(new URL(".", import.meta.url)));
const project = resolve(root, "..");
const compiler = join(project, "extension/node_modules/.bin/tsc");
const temp = await mkdtemp(join(tmpdir(), "relo-ui-proposal-"));
try {
  const result = spawnSync(compiler, [join(root, "src/app.ts"), "--strict", "--target", "ES2020", "--module", "none", "--lib", "DOM,ES2020", "--outFile", join(temp, "app.js")], { encoding: "utf8" });
  if (result.status !== 0) throw new Error(result.stdout + result.stderr);
  const [template, script] = await Promise.all([readFile(join(root, "template.html"), "utf8"), readFile(join(temp, "app.js"), "utf8")]);
  if (!template.includes("/*__APP__*/")) throw new Error("Template missing app script marker");
  const html = template.replace("/*__APP__*/", () => script);
  await writeFile(join(root, "index.html"), html);
  process.stdout.write(`Built ${join(root, "index.html")} (${Buffer.byteLength(html)} bytes)\n`);
} finally {
  await rm(temp, { recursive: true, force: true });
}
