// Runs webview/src/main.js in a browser-like sandbox (fake DOM and VS Code host) so a test can drive the real message
// handler. main.js publishes its test hook on an object in the sandbox; the harness returns it.
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function fakeElement(tag) {
  return {
    tag,
    id: "",
    textContent: "",
    innerHTML: "",
    value: "",
    disabled: false,
    hidden: false,
    scrollTop: 0,
    className: "",
    children: [],
    attributes: {},
    style: {},
    setAttribute(k, v) { this.attributes[k] = String(v); },
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attributes, k) ? this.attributes[k] : null; },
    removeAttribute(k) { delete this.attributes[k]; },
    closest() { return null; },
    matches() { return false; },
    contains() { return false; },
    addEventListener() {},
    removeEventListener() {},
    appendChild(c) { this.children.push(c); return c; },
    remove() {},
    insertAdjacentHTML(_where, html) { this.innerHTML += html; },
    querySelectorAll() { return []; },
    querySelector() { return null; },
    focus() {},
    setSelectionRange() {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  };
}

function loadBrowser() {
  const posted = [];
  const app = fakeElement("div");
  app.firstChild = null;
  app.lastChild = null;
  const store = {};
  const listeners = [];
  const document = {
    activeElement: null,
    body: fakeElement("body"),
    documentElement: fakeElement("html"),
    getElementById(id) { return id === "app" ? app : fakeElement("div"); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    createElement: (t) => fakeElement(t),
    addEventListener() {},
    execCommand() { return true; },
  };
  document.body.getAttribute = () => "chat";
  const window = {
    addEventListener(type, fn) { if (type === "message") listeners.push(fn); },
    removeEventListener() {},
  };
  // main.js stores its test hook here when it runs without module exports; it is a global of the sandbox.
  const hook = {};
  const context = vm.createContext({
    window,
    document,
    console,
    setTimeout,
    clearTimeout,
    setInterval: () => 0,
    clearInterval() {},
    acquireVsCodeApi: () => ({
      postMessage: (m) => posted.push(m),
      getState: () => store.state,
      setState: (s) => { store.state = s; },
    }),
    navigator: {},
  });
  context.__reloTestHook = hook;
  vm.runInContext(fs.readFileSync(path.join(__dirname, "main.js"), "utf8"), context, { filename: "main.js" });
  return { window, hook, get host() { return hook.mem; }, posted, send: (data) => { for (const fn of listeners) fn({ data }); } };
}

module.exports = { loadBrowser };
