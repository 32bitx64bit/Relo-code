// Stops the extension restarting a core that keeps dying. Without it a core that panics on
// startup is relaunched forever, and every launch can leave an OpenCode child behind.
export class CrashBreaker {
  private times: number[] = [];
  private open = false;

  constructor(
    private readonly limit = 3,
    private readonly windowMs = 120_000,
    private readonly now: () => number = Date.now,
  ) {}

  // Records one unexpected core exit. Returns true while restarting is still allowed.
  record(): boolean {
    const t = this.now();
    this.times = this.times.filter((x) => t - x < this.windowMs);
    this.times.push(t);
    if (this.times.length >= this.limit) this.open = true;
    return !this.open;
  }

  get tripped(): boolean {
    return this.open;
  }

  // The user asked for a restart: forget the history.
  reset(): void {
    this.times = [];
    this.open = false;
  }
}
