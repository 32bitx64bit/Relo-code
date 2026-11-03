// The memory UI needs the whole list again (window focus, a view shown again, a refresh request from a webview).
// The list source is synchronous so this module needs no vscode API and is testable under node.

/**
 * Asks for `memory.list` and hands the items back. `ok` is false when the request failed or returned no list,
 * so the caller keeps what it has instead of showing an empty list on a transient failure.
 */
export async function loadMemoryList(request: (method: string, params?: unknown) => Promise<unknown>): Promise<{ ok: boolean; items: unknown[] }> {
  const res = await request("memory.list", {});
  const items = (res as { items?: unknown } | undefined)?.items;
  return Array.isArray(items) ? { ok: true, items } : { ok: false, items: [] };
}

export const MEMORY_REFRESH_DEBOUNCE_MS = 500;

/**
 * Debounces the refresh trigger and joins the loads it starts, so focus, visibility and window-focus events arriving
 * together load the list once. `enabled` gates the trigger (relo.context.projectMemory on, core ready).
 */
export class MemoryRefresh {
  private timer?: ReturnType<typeof setTimeout>;
  private inFlight?: Promise<void>;
  // Counters the node test asserts on; they record what happened, they do not gate anything.
  private fired = 0;
  private started = 0;

  constructor(private readonly deps: { enabled: () => boolean; load: () => Promise<unknown>; debounceMs?: number }) {}

  trigger(): void {
    if (!this.deps.enabled()) return;
    if (this.timer !== undefined) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = undefined;
      this.fired += 1;
      this.run();
    }, this.deps.debounceMs ?? MEMORY_REFRESH_DEBOUNCE_MS);
  }

  /** Starts a load now, or joins the one in flight. The promise never rejects. */
  run(): Promise<void> {
    if (this.inFlight) return this.inFlight;
    if (!this.deps.enabled()) return Promise.resolve();
    this.started += 1;
    const p: Promise<void> = this.deps.load().then(
      () => undefined,
      () => undefined,
    );
    this.inFlight = p;
    void p.then(() => {
      if (this.inFlight === p) this.inFlight = undefined;
    });
    return p;
  }

  /** Debounced triggers that ran. */
  get triggeredCount(): number {
    return this.fired;
  }

  /** Loads started. */
  get startedCount(): number {
    return this.started;
  }

  /** A trigger is scheduled and has not run yet. */
  get pending(): boolean {
    return this.timer !== undefined;
  }

  dispose(): void {
    if (this.timer !== undefined) clearTimeout(this.timer);
    this.timer = undefined;
  }
}
