import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const goHandlers = readFileSync(new URL("../core/internal/app/app.go", import.meta.url), "utf8");
const goEmitters = readFileSync(new URL("../core/internal/orchestrator/coord.go", import.meta.url), "utf8");
const appEvents = readFileSync(new URL("../core/internal/app/app.go", import.meta.url), "utf8");
const protocol = readFileSync(new URL("../extension/src/protocol.ts", import.meta.url), "utf8");
const docs = readFileSync(new URL("../docs/protocol.md", import.meta.url), "utf8");

test("TypeScript protocol covers Go and documented methods and events", () => {
  const methods = [...goHandlers.matchAll(/case "([a-zA-Z][\w.]*)":/g)].map((match) => match[1]).filter((name) => name.includes("."));
  const events = [
    ...[...goEmitters.matchAll(/\.emit\("([a-z][\w.]*)"/g)].map((match) => match[1]),
    ...[...appEvents.matchAll(/(?:IPCEmit|emit)\("([a-z][\w.]*)"/g)].map((match) => match[1]),
  ];
  const methodSection = docs.match(/## (?:Selected methods|Methods)\s+([\s\S]*?)\n## Events/)?.[1] ?? "";
  const eventSection = docs.match(/## Events\s+([^\n]+)/)?.[1] ?? "";
  const methodTokens = [...methodSection.matchAll(/`([^`]+)`/g)].flatMap((match) => match[1].split(",").map((item) => item.trim()));
  const eventTokens = [...eventSection.matchAll(/`([^`]+)`/g)].flatMap((match) => match[1].split(",").map((item) => item.trim()));
  const documentedMethods = methodTokens.flatMap((token) => {
    if (token.endsWith(".*")) return methods.filter((method) => method.startsWith(token.slice(0, -1)));
    if (token.includes("|")) {
      const [prefix, ...parts] = token.split(".");
      return parts.flatMap((part) => part.split("|").map((name) => `${prefix}.${name}`));
    }
    return [token];
  });
  const documentedEvents = eventTokens.flatMap((token) => {
    if (!token.includes("|")) return [token];
    let prefix = token.slice(0, token.indexOf(".") + 1);
    return token.split("|").map((part, index) => {
      if (index === 0) {
        prefix = part.slice(0, part.lastIndexOf(".") + 1);
        return part;
      }
      if (part.includes(".")) {
        const name = `${token.slice(0, token.indexOf(".") + 1)}${part}`;
        if (part.startsWith("item.")) prefix = `${token.slice(0, token.indexOf(".") + 1)}item.`;
        return name;
      }
      return `${prefix}${part}`;
    });
  });
  assert.ok(documentedMethods.length && documentedEvents.length, "protocol documentation lists methods and events");
  for (const name of [...documentedMethods, ...documentedEvents]) {
    assert.ok(protocol.includes(`"${name}"`), `TypeScript protocol missing documented name ${name}`);
  }
  for (const method of methods) {
    assert.ok(protocol.includes(`"${method}"`), `TypeScript protocol missing method ${method}`);
  }
  for (const event of new Set(events)) {
    assert.ok(protocol.includes(`"${event}"`), `TypeScript protocol missing event ${event}`);
  }
});
