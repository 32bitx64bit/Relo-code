import assert from "node:assert/strict";
import { test } from "node:test";
import { checkCommand, checkSessionId, checkSetting, isAllowedCommand, modelsSignature, settingsReply, type SettingsSchema } from "./settingsBridge";

const schema: SettingsSchema = {
  "relo.primaryModel": { type: "string", default: "" },
  "relo.efficiency.preset": { type: "string", enum: ["off", "auto", "max"], default: "off" },
  "relo.report.maxChars": { type: "number", minimum: 1, maximum: 10000, default: 1200 },
  "relo.autoVerify": { type: "boolean", default: true },
  "relo.routing.byDifficulty": { type: "object", default: {} },
  "relo.providers": { type: "array", default: [] },
  "other.key": { type: "string", default: "" },
};

test("settings: unknown key rejected", () => {
  assert.equal(checkSetting("relo.nope", "x", schema).ok, false);
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
  assert.equal(modelsSignature(undefined), "");
  assert.equal(modelsSignature([null, 3]).length > 0, true); // junk entries do not throw
});
