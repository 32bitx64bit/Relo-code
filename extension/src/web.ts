// relo_web for the extension: custom search provider discovery with user confirmation, the web status view, and the
// browser challenge prompt. Pure helpers (parsing, message building) sit at the top so tsx --test can cover them.
//
// Nothing here lets an agent change relo.web.*: discovery reads the URL the user typed, and only the user's confirm
// button writes relo.web.customSearchTemplate, at Global scope, through writeSetting.


export type WebDiscoverReply = { template?: unknown; origin?: unknown; format?: unknown; source?: unknown; provider?: unknown; error?: unknown };
export type WebDiscovery = { template: string; origin: string; format: string; source: string; provider: string; error: string };


export type WebStatus = { ready?: unknown; mode?: unknown; browserPath?: unknown; provider?: unknown; tabs?: unknown; agents?: unknown };
export type WebStatusView = { ready: boolean; mode: string; browserPath: string; provider: string; tabs: number; agents: number };

const MAX_URL = 8192;

function text(value: unknown, max: number): string {
  return typeof value === "string" ? value.slice(0, max) : "";
}

// A discovered search URL is only usable with a %s the engine can fill; anything else would search for "%s" literally.
export function looksFillable(template: string): boolean {
  return template.includes("%s");
}

// Reads a web.discover reply. A failure arrives as a message in error with no template, not as a rejected call.
export function parseDiscovery(raw: unknown): WebDiscovery {
  const r = (raw ?? {}) as WebDiscoverReply;
  return {
    template: text(r.template, MAX_URL),
    origin: text(r.origin, MAX_URL),
    format: text(r.format, 40),
    source: text(r.source, 40),
    provider: text(r.provider, 40),
    error: text(r.error, 1000),
  };
}

export function discoveryUsable(d: WebDiscovery): boolean {
  return !!d.template && looksFillable(d.template) && !d.error;
}

// The question put to the user before relo.web.customSearchTemplate is written.
export function discoveryPrompt(d: WebDiscovery): string {
  const lines = [`Relo found this search template for ${d.origin || "the custom provider"}:`];
  lines.push(d.template);
  const kind = [d.format && `results as ${d.format}`, d.source && `found from the page's ${d.source}`].filter(Boolean).join(", ");
  if (kind) lines.push(`(${kind})`);
  lines.push("Use it for relo_web search? Only this site's origin is exempt from the local-network block.");
  return lines.join("\n");
}

export function formatStatus(s: WebStatusView): string {
  const where = s.browserPath || "browser not found";
  return [
    `Ready: ${s.ready ? "yes" : "no"}`,
    `Mode: ${s.mode || "auto"}`,
    `Provider: ${s.provider || "unknown"}`,
    `Browser: ${where}`,
    `Open tabs: ${s.tabs}`,
    `Agents browsing: ${s.agents}`,
  ].join("\n");
}

export function parseStatus(raw: unknown): WebStatusView {
  const r = (raw ?? {}) as WebStatus;
  const count = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : 0);
  return {
    ready: r.ready === true,
    mode: text(r.mode, 40),
    browserPath: text(r.browserPath, MAX_URL),
    provider: text(r.provider, 40),
    tabs: count(r.tabs),
    agents: count(r.agents),
  };
}
