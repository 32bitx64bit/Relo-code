// Parse permission.asked into a browser challenge prompt only when the payload really is one. Nothing here trusts
// the payload's types: an unexpected type collapses to a safe default instead of reaching the user as junk.

export type ChallengePermission = { id: string; agentId: string; url: string; title: string };

const MAX_URL = 8192;
const MAX_AGENT = 200;
const MAX_TITLE = 400;
const ID_RE = /^[A-Za-z0-9_.-]{1,200}$/;

function text(value: unknown, max: number): string {
  return typeof value === "string" ? value.slice(0, max) : "";
}

export function checkWebChallenge(params: unknown): ChallengePermission | undefined {
  const p = (params ?? {}) as { id?: unknown; kind?: unknown; agentId?: unknown; url?: unknown; title?: unknown };
  if (p.kind !== "web-challenge") return undefined;
  if (typeof p.id !== "string" || !ID_RE.test(p.id)) return undefined;
  const agentId = typeof p.agentId === "string" && p.agentId.length <= MAX_AGENT ? p.agentId : "an agent";
  const url = text(p.url, MAX_URL);
  const title = text(p.title, MAX_TITLE) || "Browser challenge needs you";
  return { id: p.id, agentId, url, title };
}

export function challengeMessage(c: ChallengePermission): string {
  const who = c.agentId && c.agentId !== "an agent" ? `agent ${c.agentId}` : "an agent";
  return `Relo: a browser challenge from ${who} needs you.`;
}

// Only these two answers exist: anything else means the user closed the prompt, which is a rejection.
// A challenge is site state, so an accept is always "once" and never a standing rule.
export function challengeResponse(choice: string | undefined): "once" | "reject" {
  return choice === "Done" ? "once" : "reject";
}
