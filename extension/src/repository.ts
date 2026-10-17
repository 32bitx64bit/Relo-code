import { execFile } from "node:child_process";
import { promisify } from "node:util";
import * as path from "node:path";

const exec = promisify(execFile);

async function git(root: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec("git", ["--no-optional-locks", "-C", root, ...args], { timeout: 3000, maxBuffer: 1024 * 1024 });
  return stdout.trim();
}

// Empty selection keeps fallback; otherwise the enclosing Git root, or undefined when the folder is not in a Git repository.
export async function snapRepository(selected: string, fallback: string): Promise<string | undefined> {
  if (!selected) return fallback;
  return git(path.resolve(selected), "rev-parse", "--show-toplevel").catch(() => undefined);
}

export async function repositoryInfo(root: string): Promise<{ root: string; gitRoot?: string; branch: string }> {
  try {
    const gitRoot = await git(root, "rev-parse", "--show-toplevel");
    const branch = await git(root, "symbolic-ref", "--quiet", "--short", "HEAD")
      .catch(async () => `detached HEAD (${await git(root, "rev-parse", "--short", "HEAD")})`);
    return { root, gitRoot, branch };
  } catch {
    return { root, branch: "Not a Git repository" };
  }
}
