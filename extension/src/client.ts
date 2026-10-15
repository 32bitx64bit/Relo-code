import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { EventEmitter } from "node:events";
import * as readline from "node:readline";
import { PROTOCOL_VERSION, type Frame } from "./protocol";

export class CoreClient extends EventEmitter {
  private child?: ChildProcessWithoutNullStreams;
  private seq = 0;
  private pending = new Map<string, { resolve: (v: unknown) => void; reject: (e: Error) => void }>();
  ready = false;
  private stopping = new WeakSet<ChildProcessWithoutNullStreams>();

  get running(): boolean {
    return !!this.child && !this.child.killed && this.child.exitCode === null;
  }

  constructor(private log: (s: string) => void) {
    super();
  }

  start(bin: string, args: string[], cwd?: string): void {
    if (this.running) throw new Error("relo-core is already running");
    this.log(`starting ${bin} ${args.join(" ")}`);
    this.child = spawn(bin, args, { cwd, stdio: ["pipe", "pipe", "pipe"] });
    const child = this.child;
    let finished = false;
    const finish = (code: number | null, signal?: NodeJS.Signals | null) => {
      if (finished) return;
      finished = true;
      this.ready = false;
      const intentional = this.stopping.has(child);
      this.emit("exit", { code, signal, intentional });
      for (const [, p] of this.pending) p.reject(new Error("relo-core exited"));
      this.pending.clear();
      if (this.child === child) this.child = undefined;
    };
    child.on("error", (err) => {
      this.emit("error", err);
      finish(null);
    });
    child.on("exit", (code, signal) => finish(code, signal));
    const rl = readline.createInterface({ input: child.stdout });
    rl.on("line", (line) => this.onLine(line));
    child.stderr.on("data", (buf: Buffer) => {
      const text = buf.toString();
      this.log(text.trimEnd());
      this.emit("stderr", text);
    });
  }

  private onLine(line: string): void {
    if (!line.trim()) return;
    let f: Frame;
    try {
      f = JSON.parse(line) as Frame;
    } catch {
      this.log(`bad ipc: ${line}`);
      return;
    }
    if (f.kind === "evt") {
      this.emit("event", f.method, f.params);
      this.emit(f.method ?? "event", f.params);
      return;
    }
    if ((f.kind === "res" || f.kind === "err") && f.id) {
      const p = this.pending.get(f.id);
      if (!p) return;
      this.pending.delete(f.id);
      if (f.kind === "err") p.reject(new Error(f.error?.message ?? "error"));
      else p.resolve(f.result);
    }
  }

  request<T>(method: string, params?: unknown): Promise<T> {
    const id = String(++this.seq);
    const frame: Frame = { v: PROTOCOL_VERSION, kind: "req", id, method, params };
    const request = new Promise<T>((resolve, reject) => {
      if (!this.child?.stdin.writable || this.child.exitCode !== null) {
        reject(new Error("relo-core is not running"));
        return;
      }
      this.pending.set(id, { resolve: (v) => resolve(v as T), reject });
      this.child.stdin.write(JSON.stringify(frame) + "\n", (err) => {
        if (err) {
          this.pending.delete(id);
          reject(err);
        }
      });
    });
    void request.catch(() => {});
    return request;
  }

  async dispose(): Promise<void> {
    const child = this.child;
    if (!child) return;
    this.stopping.add(child);
    try {
      if (this.ready && child.stdin.writable) child.stdin.write(encodeFrame("shutdown", {}, String(++this.seq)) + "\n");
    } catch {
      // Exit may race with extension shutdown.
    }
    if (child.exitCode === null) {
      await Promise.race([
        new Promise<void>((resolve) => child.once("exit", () => resolve())),
        new Promise<void>((resolve) => setTimeout(resolve, 2000)),
      ]);
    }
    if (child.exitCode === null) child.kill();
    if (this.child === child) this.child = undefined;
  }
}

export function encodeFrame(method: string, params: unknown, id: string): string {
  return JSON.stringify({ v: PROTOCOL_VERSION, kind: "req", id, method, params });
}

export function decodeFrame(line: string): Frame {
  return JSON.parse(line) as Frame;
}
