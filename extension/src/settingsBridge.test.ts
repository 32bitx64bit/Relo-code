import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import { CORE_SETTING_SECTIONS, checkCommand, checkSessionId, checkSetting, isAllowedCommand, modelsSignature, settingsReply, type SettingsSchema } from "./settingsBridge";

const schema: SettingsSchema = {
  "relo.primaryModel": { type: "string", default: "" },
  "relo.efficiency.preset": { type: "string", enum: ["off", "auto", "max"], default: "off" },
  "relo.report.maxChars": { type: "number", minimum: 1, maximum: 10000, default: 1200 },
  "relo.autoVerify": { type: "boolean", default: true },
  "relo.routing.byDifficulty": { type: "object", default: {} },
  "relo.providers": { type: "array", default: [] },
  "other.key": { type: "string", default: "" },
};

test("workspace mode: the manifest default is virtual, and the extension fallback matches it", () => {
  const manifest = JSON.parse(readFileSync("package.json", "utf8"));
  const actual: SettingsSchema = manifest.contributes.configuration.properties;
  assert.equal(actual["relo.workspace.mode"].default, "virtual");
  assert.deepEqual(actual["relo.workspace.mode"].enum, ["worktree", "virtual", "auto"]);
  // extension.ts sends this mode to the core; it must not drift from the manifest default.
  const source = readFileSync("src/extension.ts", "utf8");
  assert.match(source, /workspaceMode: c\.get<string>\("workspace\.mode", "virtual"\)/);
});

test("settings: unknown key rejected", () => {
  assert.equal(checkSetting("relo.nope", "x", schema).ok, false);
});

test("changes settings and repository command match the extension manifest", () => {
  const manifest = JSON.parse(readFileSync("package.json", "utf8"));
  const actual: SettingsSchema = manifest.contributes.configuration.properties;
  assert.equal(actual["relo.changes.autoApply"].default, false);
  assert.equal(actual["relo.repository"].default, "");
  assert.equal(checkSetting("relo.changes.autoApply", true, actual).ok, true);
  assert.equal(checkSetting("relo.changes.autoApply", "true", actual).ok, false);
  assert.equal(checkSetting("relo.repository", "/work/repository", actual).ok, true);
  assert.equal(checkSetting("relo.repository", {}, actual).ok, false);
  assert.deepEqual(checkCommand({ command: "relo.selectRepository" }), { ok: true, command: "relo.selectRepository", args: [] });
  assert.ok(manifest.contributes.commands.some((c: { command: string }) => c.command === "relo.selectRepository"));
});

test("path-list settings are declared as string arrays with the core's defaults", () => {
  const manifest = JSON.parse(readFileSync("package.json", "utf8"));
  const actual: SettingsSchema = manifest.contributes.configuration.properties;
  assert.deepEqual(actual["relo.changes.ignorePaths"].default, [".relo/**", "**/obj/**", "**/bin/Debug/**", "**/bin/Release/**"]);
  assert.deepEqual(actual["relo.changes.unionPaths"].default, ["*.md", "*.txt"]);
  assert.deepEqual(actual["relo.changes.takeLatestPaths"].default, []);
  for (const key of ["relo.changes.ignorePaths", "relo.changes.unionPaths", "relo.changes.takeLatestPaths"]) {
    assert.equal(actual[key].type, "array", key);
    assert.equal(checkSetting(key, ["**/obj/**"], actual).ok, true, key);
    assert.equal(checkSetting(key, "*.md", actual).ok, false, key);
  }
});

test("settings: non-relo key rejected", () => {
  assert.equal(checkSetting("other.key", "x", schema).ok, false);
  assert.equal(checkSetting("relo.__proto__", "x", schema).ok, false);
  assert.equal(checkSetting("constructor", "x", schema).ok, false);
  assert.equal(checkSetting(42, "x", schema).ok, false);
});

test("settings: wrong type rejected", () => {
  assert.equal(checkSetting("relo.primaryModel", 1, schema).ok, false);
  assert.equal(checkSetting("relo.autoVerify", "true", schema).ok, false);
  assert.equal(checkSetting("relo.report.maxChars", "5", schema).ok, false);
  assert.equal(checkSetting("relo.report.maxChars", NaN, schema).ok, false);
  assert.equal(checkSetting("relo.routing.byDifficulty", [], schema).ok, false);
  assert.equal(checkSetting("relo.routing.byDifficulty", null, schema).ok, false);
});

test("settings: enum violation rejected", () => {
  assert.equal(checkSetting("relo.efficiency.preset", "turbo", schema).ok, false);
});

test("settings: min/max violation rejected", () => {
  assert.equal(checkSetting("relo.report.maxChars", 0, schema).ok, false);
  assert.equal(checkSetting("relo.report.maxChars", 10001, schema).ok, false);
});

test("settings: valid value accepted", () => {
  const r = checkSetting("relo.efficiency.preset", "auto", schema);
  assert.deepEqual(r, { ok: true, subkey: "efficiency.preset", value: "auto" });
  assert.equal(checkSetting("relo.report.maxChars", 1, schema).ok, true);
  assert.equal(checkSetting("relo.report.maxChars", 10000, schema).ok, true);
  assert.equal(checkSetting("relo.autoVerify", false, schema).ok, true);
  assert.equal(checkSetting("relo.routing.byDifficulty", { hard: "x" }, schema).ok, true);
});

test("settings: oversized string rejected", () => {
  assert.equal(checkSetting("relo.primaryModel", "a".repeat(4001), schema).ok, false);
});

test("command allowlist accepts listed ids", () => {
  for (const id of ["relo.newSession", "relo.stopAll", "relo.agent.stop", "relo.changes.diff"]) {
    assert.equal(isAllowedCommand(id), true, id);
  }
});

test("command allowlist rejects other ids", () => {
  for (const id of ["workbench.action.terminal.new", "relo.__proto__", "constructor", "", 7, null, undefined]) {
    assert.equal(isAllowedCommand(id), false, String(id));
  }
});

test("checkCommand: id commands need non-empty string id", () => {
  assert.deepEqual(checkCommand({ command: "relo.agent.stop", id: "a1" }), {
    ok: true,
    command: "relo.agent.stop",
    args: [{ id: "a1" }],
  });
  assert.equal(checkCommand({ command: "relo.agent.stop" }).ok, false);
  assert.equal(checkCommand({ command: "relo.agent.stop", id: "" }).ok, false);
  assert.equal(checkCommand({ command: "relo.agent.stop", id: "x".repeat(201) }).ok, false);
});

test("checkCommand: rejects disallowed command", () => {
  assert.equal(checkCommand({ command: "workbench.action.terminal.new" }).ok, false);
  assert.equal(checkCommand({ command: "relo.__proto__" }).ok, false);
});

test("settingsReply: only relo.* keys, defaults filled", () => {
  const r = settingsReply({ "relo.primaryModel": "p/m" }, schema, []);
  assert.deepEqual(Object.keys(r.schema).sort(), Object.keys(r.values).sort());
  assert.equal(r.values["relo.primaryModel"], "p/m");
  assert.equal(r.values["relo.report.maxChars"], 1200);
  assert.equal("other.key" in r.values, false);
});

test("settings: array accepted within limits", () => {
  assert.equal(checkSetting("relo.providers", [], schema).ok, true);
  assert.equal(checkSetting("relo.providers", [{ id: "a" }], schema).ok, true);
  assert.equal(checkSetting("relo.providers", Array.from({ length: 50 }, () => 1), schema).ok, true);
});

test("settings: array rejected for non-array, too long, or too large", () => {
  assert.equal(checkSetting("relo.providers", { length: 0 }, schema).ok, false);
  assert.equal(checkSetting("relo.providers", "[]", schema).ok, false);
  assert.equal(checkSetting("relo.providers", null, schema).ok, false);
  assert.equal(checkSetting("relo.providers", Array.from({ length: 51 }, () => 1), schema).ok, false);
  assert.equal(checkSetting("relo.providers", [{ x: "y".repeat(200000) }], schema).ok, false);
});

test("settings: array with cyclic or unserializable content rejected", () => {
  const cyc: unknown[] = [];
  cyc.push(cyc);
  assert.equal(checkSetting("relo.providers", cyc, schema).ok, false);
});

test("webview may open history and decide all changes at once, with no id", () => {
  for (const command of ["relo.sessionHistory", "relo.changes.acceptAll", "relo.changes.rejectAll"]) {
    assert.deepEqual(checkCommand({ command }), { ok: true, command, args: [] });
  }
});

test("checkSessionId: core-style ids pass, anything else is dropped", () => {
  assert.equal(checkSessionId("sess_14acf115af1fd52c14ff"), "sess_14acf115af1fd52c14ff");
  for (const bad of ["", "a/b", "../x", "a b", "x".repeat(201), 5, null, undefined, {}]) {
    assert.equal(checkSessionId(bad), undefined, String(bad));
  }
});

test("models signature: changes when a model is added, or its limits or efforts change, and not otherwise", () => {
  const a = [{ provider: "9router", id: "cc/haiku", context: 200000, output: 8000, efforts: ["low", "high"] }];
  const same = [{ provider: "9router", id: "cc/haiku", context: 200000, output: 8000, efforts: ["low", "high"], name: "renamed" }];
  assert.equal(modelsSignature(a), modelsSignature(same));
  assert.notEqual(modelsSignature(a), modelsSignature([...a, { provider: "9router", id: "cx/new" }]));
  assert.notEqual(modelsSignature(a), modelsSignature([{ ...a[0], context: 100000 }]));
  assert.notEqual(modelsSignature(a), modelsSignature([{ ...a[0], efforts: ["low"] }]));
  assert.notEqual(modelsSignature(a), modelsSignature([{ ...a[0], vision: "yes" }]));
  assert.notEqual(modelsSignature([{ ...a[0], vision: "yes", visionSource: "name" }]), modelsSignature([{ ...a[0], vision: "yes", visionSource: "configured" }]));
  assert.equal(modelsSignature(undefined), "");
  assert.equal(modelsSignature([null, 3]).length > 0, true); // junk entries do not throw
});

// A button the webview draws but the bridge drops does nothing and says nothing. Every command the webview can post
// must pass the check, an id command with the id the button carries and any other without one.
test("every command the webview posts passes the bridge", () => {
  const source = readFileSync("../webview/src/main.js", "utf8");
  const registered = new Set<string>([...readFileSync("src/extension.ts", "utf8").matchAll(/\bcmd\("(relo\.[\w.]+)"/g)].map((m) => m[1]));
  const withId = new Set<string>();
  for (const m of source.matchAll(/rowBtn\("command",\s*"(relo\.[\w.]+)"/g)) withId.add(m[1]);
  for (const m of source.matchAll(/data-command="(relo\.[\w.]+)"/g)) withId.add(m[1]);
  const plain = new Set<string>();
  for (const m of source.matchAll(/data-(?:cmd|menu-cmd)="(relo\.[\w.]+)"/g)) plain.add(m[1]);
  assert.ok(withId.has("relo.changes.accept") && withId.has("relo.changes.acceptWithParents"), "the scan found the Changes buttons");
  for (const command of withId) {
    assert.deepEqual(checkCommand({ command, id: "chg_1" }), { ok: true, command, args: [{ id: "chg_1" }] }, command);
    assert.ok(registered.has(command), `${command} is not registered in extension.ts`);
  }
  for (const command of plain) {
    assert.deepEqual(checkCommand({ command }), { ok: true, command, args: [] }, command);
    assert.ok(registered.has(command), `${command} is not registered in extension.ts`);
  }
});

// A setting the core reads but the extension does not watch is saved and then ignored until the core restarts: that is
// how permissions set to allow kept asking in a running window. Every key in the payload must re-apply when it changes.
test("every setting sent to the core re-applies when it changes", () => {
  const source = readFileSync("src/extension.ts", "utf8");
  const start = source.indexOf("async function settingsPayload");
  const body = source.slice(start, source.indexOf("\n}\n", start));
  const keys = [...body.matchAll(/\bc\.get(?:<.*?>)?\(\s*"([\w.]+)"/g)].map((m) => `relo.${m[1]}`);
  assert.ok(keys.includes("relo.permissions.destructive") && keys.includes("relo.permissions.yolo"), "the scan found the permission settings");
  assert.ok(keys.length > 50, `the scan found only ${keys.length} settings`);
  const covered = (key: string) => CORE_SETTING_SECTIONS.some((section) => key === section || key.startsWith(`${section}.`));
  assert.deepEqual(keys.filter((key) => !covered(key)), []);
});
