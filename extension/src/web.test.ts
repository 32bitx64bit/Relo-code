import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import { challengeMessage, challengeResponse, checkWebChallenge } from "./webChallenge";
import { discoveryPrompt, discoveryUsable, formatStatus, looksFillable, parseDiscovery, parseStatus } from "./web";

test("discovery reply is read from web.discover and must carry a fillable template", () => {
  const d = parseDiscovery({ template: "https://s.example/search?q=%s", origin: "https://s.example", format: "html", source: "form", provider: "custom", error: "" });
  assert.deepEqual(d, { template: "https://s.example/search?q=%s", origin: "https://s.example", format: "html", source: "form", provider: "custom", error: "" });
  assert.equal(discoveryUsable(d), true);
  assert.equal(looksFillable("https://s.example/search?q=%s"), true);
  assert.equal(discoveryUsable({ ...d, template: "https://s.example/search" }), false);
  assert.equal(discoveryUsable({ ...d, error: "no form found" }), false);
  // Undefined, wrong types and a failed call's shape resolve to an empty error-and-nothing reply, never a throw.
  assert.equal(parseDiscovery(undefined).template, "");
  assert.equal(parseDiscovery({ template: 42 }).template, "");
  const prompt = discoveryPrompt(d);
  assert.ok(prompt.includes("https://s.example/search?q=%s"));
  assert.ok(prompt.includes("html"));
  assert.ok(prompt.includes("local-network"));
});

test("web.status view defaults every missing field", () => {
  assert.deepEqual(parseStatus(undefined), { ready: false, mode: "", browserPath: "", provider: "", tabs: 0, agents: 0 });
  const s = parseStatus({ ready: true, mode: "hidden", browserPath: "/usr/bin/chromium", provider: "custom", tabs: 2, agents: 1 });
  assert.equal(s.ready, true);
  assert.equal(s.tabs, 2);
  assert.ok(formatStatus(s).includes("Open tabs: 2"));
  assert.ok(formatStatus(parseStatus({})).includes("browser not found"));
});

test("only a real web-challenge permission is prompted, and only Done accepts", () => {
  const c = checkWebChallenge({ id: "web-artist-3", kind: "web-challenge", agentId: "artist", url: "https://site.example/c", title: "Browser challenge needs you" });
  assert.deepEqual(c, { id: "web-artist-3", agentId: "artist", url: "https://site.example/c", title: "Browser challenge needs you" });
  assert.equal(checkWebChallenge({ id: "p1", kind: "bash", agentId: "a" }), undefined);
  assert.equal(checkWebChallenge({ id: "bad id", kind: "web-challenge" }), undefined);
  assert.equal(checkWebChallenge(undefined), undefined);
  // A payload rebuilt with a numeric url is a different shape; only the strings it carries are used.
  assert.equal(checkWebChallenge({ id: c.id, kind: "web-challenge", url: 12 })?.url, "");
  assert.equal(checkWebChallenge({ id: c!.id, kind: "web-challenge", agentId: 7 })?.agentId, "an agent");
  assert.ok(challengeMessage(c!).includes("agent artist"));
  assert.equal(challengeResponse("Done"), "once");
  assert.equal(challengeResponse("Cancel"), "reject");
  assert.equal(challengeResponse(undefined), "reject");
});

test("web settings stay user-owned: commands declared and no agent path writes them", () => {
  const pkg = JSON.parse(readFileSync("package.json", "utf8"));
  const commands = pkg.contributes.commands as { command: string; title: string }[];
  const titles = new Map(commands.map((c) => [c.command, c.title]));
  assert.equal(titles.get("relo.web.redetect"), "Relo: Re-detect search provider");
  assert.equal(titles.get("relo.web.status"), "Relo: Web status");
  // The only write of the confirmed template is writeSetting from webFlows.ts, after the modal confirm.
  const web = readFileSync("src/webFlows.ts", "utf8");
  assert.equal((web.match(/"web\.customSearchTemplate"/g) ?? []).length, 1);
  assert.ok(web.indexOf('"Use this template"') < web.indexOf('writeSetting("web.customSearchTemplate"'));
  for (const key of ["relo.web.mode", "relo.web.searchProvider", "relo.web.autoCheckbox", "relo.web.customSearchUrl", "relo.web.customSearchTemplate"]) {
    assert.equal(pkg.contributes.configuration.properties[key].scope, "application");
  }
});
