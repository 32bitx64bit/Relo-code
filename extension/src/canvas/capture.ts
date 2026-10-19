import { access } from "node:fs/promises";
import type { CanvasPreviewServer } from "./preview";
import type { CanvasDiagnostic } from "./types";

export interface CanvasCaptureResult { png: Buffer; pngBase64: string; width: number; height: number; errors: CanvasDiagnostic[]; visualInspectionAvailable: boolean; inspection?: string }

export class CanvasCaptureService {
  private active = 0;
  constructor(private readonly preview: CanvasPreviewServer, private readonly options: { executablePath?: string; inspect?: (png: Buffer) => Promise<string> } = {}) {}

  async capture(id: string, size: { width?: number; height?: number } = {}): Promise<CanvasCaptureResult> {
    if (this.active >= 2) throw new Error("Canvas capture is busy; at most two captures may run concurrently");
    this.active++;
    try {
      const candidates = [this.options.executablePath, process.env.RELO_CANVAS_BROWSER, process.env.CHROMIUM_PATH, "/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", process.env.PROGRAMFILES ? `${process.env.PROGRAMFILES}/Google/Chrome/Application/chrome.exe` : undefined].filter((item): item is string => !!item);
      let executablePath: string | undefined;
      for (const candidate of candidates) { try { await access(candidate); executablePath = candidate; break; } catch { /* try next installed browser */ } }
      if (!executablePath) throw new Error("No installed Chromium browser found. Set RELO_CANVAS_BROWSER to an installed Chromium executable; Canvas never downloads a browser.");
      const width = Math.max(320, Math.min(1920, size.width ?? 1280));
      const height = Math.max(240, Math.min(1440, size.height ?? 900));
      const url = await this.preview.url(id);
      const origin = new URL(url).origin;
      const { chromium } = await import("playwright-core");
      const browser = await chromium.launch({ executablePath, headless: true, chromiumSandbox: true, timeout: 15_000 });
      try {
        const context = await browser.newContext({ viewport: { width, height }, acceptDownloads: false, serviceWorkers: "block" });
        await context.route("**/*", (route) => {
          const request = new URL(route.request().url());
          return request.origin === origin || ["data:", "blob:", "about:"].includes(request.protocol) ? route.continue() : route.abort();
        });
        const page = await context.newPage();
        const errors: CanvasDiagnostic[] = [];
        page.on("pageerror", (error) => { if (errors.length < 20) errors.push({ code: "runtime", message: error.message.slice(0, 2000) }); });
        await page.goto(url, { waitUntil: "domcontentloaded", timeout: 15_000 });
        await page.waitForFunction(() => (globalThis as unknown as { __canvasReady?: boolean }).__canvasReady === true, undefined, { timeout: 5000 }).catch((error: unknown) => { errors.push({ code: "render_timeout", message: `Canvas did not report ready before capture: ${error instanceof Error ? error.message : String(error)}` }); });
        const frame = page.frames().find((item) => item.parentFrame());
        if (frame) await frame.evaluate("document.fonts.ready.then(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))))");
        const png = await page.screenshot({ type: "png", timeout: 10_000 });
        if (png.length > 4 * 1024 * 1024) throw new Error("Canvas screenshot exceeds 4 MiB; use a smaller viewport");
        const inspection = this.options.inspect ? await this.options.inspect(png) : undefined;
        return { png, pngBase64: png.toString("base64"), width, height, errors: [...errors, ...this.preview.diagnostics(id)], visualInspectionAvailable: !!this.options.inspect, inspection };
      } finally { await browser.close(); }
    } finally { this.active--; }
  }
}
