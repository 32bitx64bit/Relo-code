import { execFile } from "node:child_process";
import { promisify } from "node:util";

const exec = promisify(execFile);

async function git(root: string, ...args: string[]): Promise<string> {
  const { stdout } = await exec("git", ["--no-optional-locks", "-C", root, ...args], { timeout: 3000, maxBuffer: 1024 * 1024 });
  return stdout.trim();
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
