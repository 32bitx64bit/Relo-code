import * as path from "node:path";

// Where the Relo core binary can come from, in order:
// 1. bin/ inside the installed extension (what `make install` packages). Independent of the open workspace.
// 2. core/ next to the extension folder (extension run from a Relo checkout).
// 3. core/ in the open workspace (only when the workspace is the Relo repository itself).
export function locateCore(
  extensionPath: string,
  ws: string,
  platform: string = process.platform,
  arch: string = process.arch,
  exists: (p: string) => boolean,
): { cmd: string; cwd?: string } {
  const osName = platform === "win32" ? "windows" : platform;
  const archName = arch === "x64" ? "amd64" : arch;
  const ext = platform === "win32" ? ".exe" : "";
  const name = `relo-core-${osName}-${archName}${ext}`;
  const bundled = path.join(extensionPath, "bin", name);
  if (exists(bundled)) return { cmd: bundled };
  const local = path.join(extensionPath, "..", "core");
  if (exists(path.join(local, "cmd", "relo-core"))) return { cmd: "go", cwd: local };
  const repo = path.join(ws, "core", "cmd", "relo-core");
  if (exists(repo)) return { cmd: "go", cwd: path.join(ws, "core") };
  throw new Error(`Relo Core binary ${name} not found in ${path.join(extensionPath, "bin")}. Reinstall with \`make install\` from the Relo repository.`);
}
