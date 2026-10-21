import assert from "node:assert/strict";
import { test } from "node:test";
import { envKeyName, sanitizeDiscovered, sanitizeProviderList, secretKey, validateProvider } from "./providers";

const good = {
  id: "acme",
  name: "Acme",
  kind: "openai",
  baseURL: "https://host/v1",
  models: [{ id: "acme-big", name: "Acme Big", context: 200000, output: 16000, reasoning: true, efforts: ["low", "medium", "high"] }],
};

test("secretKey and envKeyName follow contract section 2", () => {
  assert.equal(secretKey("acme"), "relo.providerKey.acme");
  assert.equal(envKeyName("my-prov.v2"), "RELO_KEY_MY_PROV_V2");
});

test("validateProvider accepts a good provider and keeps only known fields", () => {
  const r = validateProvider({ ...good, apiKey: "sk-secret", __proto__: { polluted: true }, extra: 1 });
  assert.equal(r.ok, true);
  if (!r.ok) return;
  assert.deepEqual(Object.keys(r.provider).sort(), ["baseURL", "id", "kind", "models", "name"]);
  assert.equal("apiKey" in r.provider, false);
  assert.deepEqual(r.provider.models[0], good.models[0]);
  assert.equal(({} as Record<string, unknown>).polluted, undefined);
});

test("validateProvider rejects bad ids", () => {
  for (const id of ["", "Acme", "-x", "a b", "a".repeat(41), "x/y", "../etc", "__proto__", "Constructor", 7, null]) {
    assert.equal(validateProvider({ ...good, id }).ok, false, String(id));
  }
});

test("validateProvider: prototype keys never pass as ids (constructor is a valid plain id)", () => {
  assert.equal(validateProvider({ ...good, id: "__proto__" }).ok, false);
  const r = validateProvider({ ...good, id: "constructor" });
  assert.equal(r.ok && r.provider.id, "constructor");
});

test("validateProvider reserves built-in provider ids", () => {
  assert.equal(validateProvider({ ...good, id: "openai" }).ok, false);
  assert.equal(validateProvider({ ...good, id: "anthropic" }).ok, false);
});

test("validateProvider rejects bad kind and baseURL", () => {
  assert.equal(validateProvider({ ...good, kind: "gemini" }).ok, false);
  assert.equal(validateProvider({ ...good, kind: undefined }).ok, false);
  for (const baseURL of ["ftp://x", "javascript:alert(1)", "not a url", "", 5, "https://u:p@host/v1", "https://" + "a".repeat(600)]) {
    assert.equal(validateProvider({ ...good, baseURL }).ok, false, String(baseURL).slice(0, 40));
  }
  assert.equal(validateProvider({ ...good, baseURL: "http://localhost:8080/v1" }).ok, true);
});

test("validateProvider rejects non-object provider and arrays", () => {
  for (const p of [null, undefined, "x", 1, [], [good]]) assert.equal(validateProvider(p).ok, false, String(p));
});

test("validateProvider model rules: ids, numbers, efforts", () => {
  const withModel = (m: unknown) => validateProvider({ ...good, models: [m] });
  assert.equal(withModel({ id: "has space" }).ok, false);
  assert.equal(withModel({ id: 'quo"te' }).ok, false);
  assert.equal(withModel({ id: "back`tick" }).ok, false);
  assert.equal(withModel({ id: "" }).ok, false);
  assert.equal(withModel({ id: "x", context: -1 }).ok, false);
  assert.equal(withModel({ id: "x", context: 1.5 }).ok, false);
  assert.equal(withModel({ id: "x", context: "200k" }).ok, false);
  assert.equal(withModel({ id: "x", output: Number.MAX_SAFE_INTEGER + 1 }).ok, false);
  assert.equal(withModel({ id: "x", efforts: "low" }).ok, false);
  assert.equal(withModel({ id: "x", efforts: [1] }).ok, false);
  assert.equal(withModel({ id: "x", efforts: ["Bad Effort"] }).ok, false);
  assert.equal(withModel({ id: "x", efforts: Array.from({ length: 11 }, (_, i) => `e${i}`) }).ok, false);
  assert.equal(withModel({ id: "x", reasoning: "yes" }).ok, false);
  const ok = withModel({ id: "x" });
  assert.equal(ok.ok && ok.provider.models[0].name, "x");
});

test("validateProvider rejects duplicate model ids and oversized model lists", () => {
  assert.equal(validateProvider({ ...good, models: [{ id: "a" }, { id: "a" }] }).ok, false);
  const many = Array.from({ length: 201 }, (_, i) => ({ id: `m${i}` }));
  assert.equal(validateProvider({ ...good, models: many }).ok, false);
  assert.equal(validateProvider({ ...good, models: "nope" }).ok, false);
});

test("validateProvider allows empty model list", () => {
  const r = validateProvider({ ...good, models: [] });
  assert.equal(r.ok && r.provider.models.length, 0);
});

test("sanitizeProviderList drops invalid, duplicate and env-colliding entries", () => {
  const r = sanitizeProviderList([good, { ...good, name: "dup" }, { ...good, id: "bad id" }, { ...good, id: "my-prov" }, { ...good, id: "my_prov" }]);
  assert.deepEqual(r.providers.map((p) => p.id), ["acme", "my-prov"]);
  assert.equal(r.errors.length, 3);
});

test("sanitizeProviderList handles hostile list shapes", () => {
  assert.deepEqual(sanitizeProviderList(undefined).providers, []);
  assert.deepEqual(sanitizeProviderList(null).providers, []);
  assert.equal(sanitizeProviderList({ 0: good }).errors.length, 1);
  assert.equal(sanitizeProviderList("x").providers.length, 0);
  const huge = Array.from({ length: 500 }, () => good);
  const r = sanitizeProviderList(huge);
  assert.ok(r.providers.length <= 1);
  assert.ok(r.errors.some((e) => e.includes("at most")));
});

test("sanitizeDiscovered keeps valid models, caps count, drops junk", () => {
  const out = sanitizeDiscovered([{ id: "ok", context: 1000 }, { id: "bad id" }, null, "x", { id: "ok" }]);
  assert.deepEqual(out, [{ id: "ok", name: "ok", context: 1000 }]);
  assert.deepEqual(sanitizeDiscovered("nope"), []);
  assert.equal(sanitizeDiscovered(Array.from({ length: 5000 }, (_, i) => ({ id: `m${i}` }))).length, 1000);
});

test("a model's vision flag is kept only as a boolean, and absent stays absent", () => {
  const out = sanitizeDiscovered([{ id: "sees", vision: true }, { id: "blind", vision: false }, { id: "silent" }, { id: "odd", vision: "yes" }]);
  assert.deepEqual(out, [{ id: "sees", name: "sees", vision: true }, { id: "blind", name: "blind", vision: false }, { id: "silent", name: "silent" }]);
});
