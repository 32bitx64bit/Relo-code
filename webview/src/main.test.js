const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { md, submit, handleKeyDown, handleKeydown, isRunning } = require("./main.js");

test("webview source exists and is not empty", () => {
  const src = fs.readFileSync(path.join(__dirname, "main.js"), "utf8");
  assert.ok(src.includes("Relo Code"));
  assert.ok(src.includes("Ask Relo"));
  assert.ok(!src.includes("TODO"));
});

test("send handler submits trimmed prompt once and clears input", () => {
  const box = { value: "  Fix the bug  " };
  const messages = [];
  submit(box, (message) => messages.push(message));
  assert.deepEqual(messages, [{ type: "submit", text: "Fix the bug" }]);
  assert.equal(box.value, "");
  submit(box, (message) => messages.push(message));
  assert.equal(messages.length, 1);
});

test("markdown escapes HTML and unsafe link syntax", () => {
  const rendered = md('<img src=x onerror="alert(1)"> [click](javascript:alert(1))');
  assert.ok(!rendered.includes("<img"));
  assert.ok(!rendered.includes('href="javascript:'));
  assert.ok(rendered.includes("&lt;img"));
});

test("Enter sends without newline, Shift+Enter does not submit", () => {
  const box = { value: "hello" };
  const messages = [];
  let prevented = false;
  const e = { key: "Enter", shiftKey: false, preventDefault: () => { prevented = true; } };
  const sent = handleKeyDown(e, box, (m) => messages.push(m));
  assert.equal(sent, true);
  assert.equal(prevented, true);
  assert.deepEqual(messages, [{ type: "submit", text: "hello" }]);
  assert.equal(box.value, "");

  const box2 = { value: "line" };
  const messages2 = [];
  const e2 = { key: "Enter", shiftKey: true, preventDefault: () => {} };
  const sent2 = handleKeyDown(e2, box2, (m) => messages2.push(m));
  assert.equal(sent2, false);
  assert.equal(messages2.length, 0);
  assert.equal(box2.value, "line");
});

test("send button state toggles with running", () => {
  assert.equal(isRunning({ agents: [{ status: "running" }] }), true);
  assert.equal(isRunning({ agents: [{ status: "completed" }], messages: [] }), false);
  assert.equal(isRunning({ agents: [], messages: [{ streaming: true }] }), true);
});

test("copy button text never includes raw HTML", () => {
  let rendered = md("```\n<script>alert(1)</script>\n```");
  assert.ok(!rendered.includes("<script>alert"));
  assert.ok(rendered.includes("&lt;script&gt;alert(1)&lt;/script&gt;"));
});

test("fence language is stripped from code and shown only in the head label", () => {
  const rendered = md("Intro\n```ts\nconst a = 1;\n```");
  assert.ok(rendered.includes('<span>ts</span>'));
  const code = rendered.match(/<pre><code>([\s\S]*?)<\/code><\/pre>/)[1];
  assert.equal(code, "const a = 1;");
  assert.ok(!/^ts\b/.test(code));
});

test("copy payload keeps real newlines and contains no <br/>", () => {
  const src = "```js\nline1\n  line2\n```";
  const rendered = md(src);
  const data = rendered.match(/data-code="([^"]*)"/)[1].replace(/&#13;/g, "\r").replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
  assert.equal(data, "line1\n  line2");
  assert.ok(!rendered.includes("<br/>") || !/<pre>[\s\S]*<br\/>[\s\S]*<\/pre>/.test(rendered));
  assert.ok(!rendered.match(/<pre><code>[\s\S]*?<\/code><\/pre>/)[0].includes("<br/>"));
  assert.ok(!rendered.match(/data-code="[^"]*"/)[0].includes("<br/>"));
});

test("HTML inside a code fence is escaped in the pre block", () => {
  const rendered = md("```html\n<b onclick=\"x()\">hi</b>\n```");
  assert.ok(!rendered.includes("<b onclick"));
  assert.ok(rendered.includes("&lt;b onclick=&quot;x()&quot;&gt;hi&lt;/b&gt;"));
});

test("javascript: link is rejected and not rendered as href", () => {
  const rendered = md("[x](javascript:alert(1))");
  assert.ok(!/href\s*=\s*["']?javascript:/i.test(rendered));
});

const { fmtTokens, plural, chatHtml, dashboardHtml, agentRowsHtml, efficiencyHtml } = require("./main.js");

test("pluralization for agents, tasks and files", () => {
  assert.equal(plural(0, "agent"), "0 agents");
  assert.equal(plural(1, "agent"), "1 agent");
  assert.equal(plural(2, "agent"), "2 agents");
  assert.equal(plural(1, "task"), "1 task");
  assert.equal(plural(3, "file"), "3 files");
  const html = dashboardHtml({ agents: [{ id: "a", role: "coder", status: "completed", usedTokens: 0 }], tasks: [], changes: [{}] });
  assert.ok(html.includes("1 agent ·"));
  assert.ok(!html.includes("1 agents"));
  assert.ok(html.includes("1 file changed"));
});

test("fmtTokens boundaries", () => {
  assert.equal(fmtTokens(0), "0");
  assert.equal(fmtTokens(950), "950");
  assert.equal(fmtTokens(999), "999");
  assert.equal(fmtTokens(1000), "1.0k");
  assert.equal(fmtTokens(12345), "12.3k");
  assert.equal(fmtTokens(57600), "57.6k");
  assert.equal(fmtTokens(999999), "1.0M");
  assert.equal(fmtTokens(1e6), "1.0M");
  assert.equal(fmtTokens(1234567), "1.2M");
  assert.equal(fmtTokens(undefined), "0");
});

test("tokens use fmtTokens everywhere (no raw 'tok' or unit suffix mixes)", () => {
  const state = {
    agents: [{ id: "a", role: "coder", status: "running", model: "m", usedTokens: 12345, contextTokens: 9000, window: 300000 }],
    tasks: [], cache: { session: { input: 12345, output: 950 } }, changes: [],
  };
  const dash = dashboardHtml(state);
  assert.ok(dash.includes("12.3k tokens"));
  assert.ok(!/\d tok\b/.test(dash));
  assert.ok(agentRowsHtml(state.agents).includes("9.0k / 300.0k"));
  const footer = chatHtml(state);
  assert.ok(footer.includes("In 12.3k"));
  assert.ok(footer.includes("Out 950"));
});

test("single mode control in chat and dashboard, no header duplicate", () => {
  const chat = chatHtml({ session: { mode: "plan" }, messages: [] });
  assert.equal((chat.match(/<select id="mode"/g) || []).length, 1);
  assert.ok(!chat.includes("mode-btn"));
  assert.ok(chat.includes('<option value="plan" selected>'));
  const header = chat.slice(0, chat.indexOf('<div class="messages"'));
  assert.ok(!header.includes("<select"));
  const dash = dashboardHtml({ messages: [] });
  assert.equal((dash.match(/<select id="mode"/g) || []).length, 1);
});

test("empty state is one plain line", () => {
  const chat = chatHtml({ messages: [] });
  assert.ok(chat.includes('<div class="empty">Describe a task to start.</div>'));
  assert.ok(!/Relo will build|task graph/.test(chat));
});

test("escalation marker renders textContent-safe 'escalated from <model>'", () => {
  const html = agentRowsHtml([{ id: "a", role: "coder", status: "running", model: "B", escalatedFrom: "<A>", usedTokens: 0, budget: 0 }]);
  assert.ok(html.includes('<div class="agent-escalated">escalated from &lt;A&gt;</div>'));
  const plain = agentRowsHtml([{ id: "b", role: "coder", status: "running", model: "B" }]);
  assert.ok(!plain.includes("agent-escalated"));
});

test("efficiency section absent when state.efficiency missing or not an object", () => {
  assert.equal(efficiencyHtml(undefined), "");
  assert.equal(efficiencyHtml(null), "");
  assert.equal(efficiencyHtml("balanced"), "");
  assert.equal(efficiencyHtml(["x"]), "");
  assert.ok(!dashboardHtml({ messages: [] }).includes("Token saving"));
});

test("efficiency section present: collapsed header, preset and role -> profile rows", () => {
  const html = efficiencyHtml({ preset: "lean", profiles: { coder: "fast-1", planner: "deep-2", bad: 7 } });
  assert.ok(html.includes(">Token saving</span>"));
  assert.ok(html.includes('aria-expanded="false"'));
  assert.ok(html.includes('class="section-body" hidden'));
  assert.ok(html.includes("<th scope=\"row\">Saving level</th><td>Lean</td>"));
  assert.ok(html.includes("<th scope=\"row\">coder</th><td>fast-1</td>"));
  assert.ok(html.includes("<th scope=\"row\">planner</th><td>deep-2</td>"));
  assert.ok(!html.includes(">bad<"));
  assert.ok(dashboardHtml({ messages: [], efficiency: { preset: "lean" } }).includes("Token saving"));
});

test("markdown still escapes HTML and rejects javascript: links", () => {
  const out = md("<b>x</b> [go](javascript:alert(1))");
  assert.ok(out.includes("&lt;b&gt;x&lt;/b&gt;"));
  assert.ok(!/href\s*=\s*["']?javascript:/i.test(out));
});

const mainApi = require("./main.js");

test("chips show running/total agents, done/total tasks, pass/total checks, context % or dash", () => {
  const s = {
    agents: [{ id: "a", status: "running" }, { id: "b", status: "completed" }, { id: "c", status: "completed" }],
    tasks: [{ status: "completed" }, { status: "running" }, { status: "queued" }],
    changes: [{}, {}, {}],
    verification: [{ status: "passed" }, { status: "failed" }],
  };
  const c = mainApi.chipCounts(s);
  assert.equal(c.agents.label, "1/3");
  assert.equal(c.tasks.label, "1/3");
  assert.equal(c.changes.label, "3");
  assert.equal(c.checks.label, "1/2");
  assert.equal(c.context.label, "-");
  const filled = mainApi.chipCounts({ agents: [{ window: 300000, contextTokens: 183000 }] });
  assert.equal(filled.context.label, "61%");
});

test("context chip measures the latest call against the window, never the running total or the planning budget", () => {
  // 94k spent over many calls, but the last call held 11k of a 300k window; the 24k planning budget is irrelevant.
  const a = { id: "p", usedTokens: 94000, budget: 24000, contextTokens: 11000, window: 300000 };
  assert.equal(mainApi.chipCounts({ agents: [a] }).context.label, "4%");
  assert.equal(mainApi.chipCounts({ agents: [{ usedTokens: 94000, budget: 24000 }] }).context.label, "-");
  assert.equal(mainApi.chipCounts({ agents: [{ contextTokens: 500000, window: 300000 }] }).context.label, "100%");
  const drawer = mainApi.drawerHtml("context", { agents: [Object.assign({ title: "Primary" }, a)] });
  assert.ok(drawer.includes("11.0k / 300.0k"));
  assert.ok(!drawer.includes("94.0k / 24.0k"));
  assert.ok(drawer.includes("94.0k tokens used in total"));
  const unknown = mainApi.drawerHtml("context", { agents: [{ title: "P", contextTokens: 8000 }] });
  assert.ok(unknown.includes("8.0k / ?"));
});

test("status strip has compact chips with open highlight and no chevrons", () => {
  const html = mainApi.stripHtml({ agents: [], tasks: [] });
  assert.ok(!html.includes("codicon-chevron"));
  const open = mainApi.stripHtml({ agents: [], tasks: [] });
  assert.equal((open.match(/aria-expanded="true"/g) || []).length, 0);
});

test("system message renders info icon, no uppercase role label", () => {
  const html = mainApi.chatHtml({ messages: [{ id: "m", role: "system", text: "Workspace is not a Git repository." }] });
  assert.ok(html.includes("codicon-info"));
  assert.ok(html.includes("msg-text sys"));
  assert.ok(!html.includes(">system<"));
  assert.ok(!html.includes("msg-role"));
});

test("composer model button shows model id tail with chevron, no 'Model ·' prefix", () => {
  const html = mainApi.chatHtml({ backend: { primary: "openai/gpt-5", models: [] }, messages: [] });
  assert.ok(html.includes(">gpt-5<") || html.includes(">gpt-5"));
  assert.ok(html.includes("codicon-chevron-down"));
  assert.ok(!html.includes("Model ·"));
  const none = mainApi.chatHtml({ backend: {}, messages: [] });
  assert.ok(none.includes("Select model"));
});

test("settings page: single back arrow in toolbar, title says Settings", () => {
  const sp = mainApi.settingsPageHtml({ schema: {}, values: {}, defaults: {}, models: [] }, "");
  assert.ok(!sp.includes("codicon-arrow-left"));
  assert.ok(!sp.includes('data-page="chat"'));
  const tb = mainApi.toolbarHtml({ session: { objective: "Some task" } });
  assert.ok(tb.includes("Some task"));
  assert.ok(tb.includes('data-page="settings"'));
});

test("settings open-in-VS-Code is a text link, restart stays secondary", () => {
  const sp = mainApi.settingsPageHtml({ schema: {}, values: {}, defaults: {}, models: [] }, "");
  assert.ok(/class="text-link"[^>]*data-settings-open/.test(sp));
  assert.ok(/class="secondary"[^>]*data-cmd="relo.restartBackend"/.test(sp));
});

test("changes drawer: letter decoration, applied badge, disabled accept/reject with tooltip", () => {
  const html = mainApi.drawerHtml("changes", {
    changes: [
      { id: "c1", path: "a.ts", status: "modified" },
      { id: "c2", path: "b.ts", status: "added" },
      { id: "c3", path: "c.ts", status: "applied" },
    ],
  });
  assert.ok(html.includes('class="letter M"'));
  assert.ok(html.includes('class="letter A"'));
  assert.ok(html.includes('class="badge" title="applied"'));
  assert.ok(html.includes("Applied directly; cannot revert"));
  assert.equal((html.match(/disabled/g) || []).length, 2);
});

test("context drawer: per-agent used/budget and cache hit rate from state.cache", () => {
  const html = mainApi.drawerHtml("context", {
    agents: [{ id: "a1", title: "Primary", generation: 2, usedTokens: 90000, contextTokens: 42000, window: 100000 }],
    cache: { session: { hitRate: 0.94 }, agents: [{ agentId: "a1", hitRate: 0.9 }] },
  });
  assert.ok(html.includes("Cache hit 94%"));
  assert.ok(html.includes("42.0k / 100.0k"));
  assert.ok(html.includes("cache 90%"));
  assert.ok(html.includes('class="agent-bar" style="width:42%"'));
});

const modelsFx = [
  { id: "claude-opus-4", provider: "anthropic", context: 200000, efforts: ["low", "medium", "high"] },
  { id: "acme-mini", provider: "acme", context: 32000 },
  { id: "bare", provider: "acme" },
];
const schemaFx = Object.fromEntries(["relo.routing.mode", "relo.primaryModel", "relo.tier1Model", "relo.tier2Model", "relo.routing.escalate", "relo.models.contextLimit", "relo.models.efforts"].map((k) => [k, { description: k }]));
const cacheFx = (over) => ({ schema: schemaFx, defaults: {}, models: modelsFx, values: Object.assign({ "relo.routing.mode": "simple", "relo.providers": [] }, over || {}) });

test("settings: simple mode shows tiers, hides difficulty table; advanced does the reverse", () => {
  const simple = mainApi.settingsPageHtml(cacheFx(), "");
  assert.ok(simple.includes('data-mode="simple"') && simple.includes('aria-checked="true"'));
  assert.ok(simple.includes("Tier 1 model") && simple.includes("Tier 2 model"));
  assert.ok(!simple.includes("Routing by difficulty"));
  const adv = mainApi.settingsPageHtml(cacheFx({ "relo.routing.mode": "advanced" }), "");
  assert.ok(adv.includes("Routing by difficulty") && adv.includes("Primary model"));
  assert.ok(!adv.includes("Tier 1 model") && !adv.includes("Tier 2 model"));
  assert.ok(mainApi.settingsPageHtml(cacheFx({ "relo.routing.mode": "bogus" }), "").includes("Tier 1 model"), "unknown mode falls back to simple");
});

test("settings: the compaction share is a bounded number on the Context page", () => {
  const cache = cacheFx({ "relo.context.compactAt": 0.9 });
  cache.schema = { ...schemaFx, "relo.context.compactAt": { type: "number", minimum: 0, maximum: 1, description: "share" } };
  const html = mainApi.settingsPageHtml(cache, "");
  assert.ok(html.includes("Compact at"), "the row is listed in a group, so it is shown");
  assert.match(html, /min="0"[^>]*max="1"|max="1"[^>]*min="0"/, "a share outside 0 to 1 cannot be typed in");
  assert.ok(html.includes('value="0.9"'));
});

test("model hint: ctx and effort list, nothing when unknown", () => {
  assert.equal(mainApi.modelHint(modelsFx[0]), "ctx 200.0k · low/med/high");
  assert.equal(mainApi.modelHint(modelsFx[1]), "ctx 32.0k");
  assert.equal(mainApi.modelHint(modelsFx[2]), "");
  assert.equal(mainApi.modelHint(null), "");
});

test("effective window follows the core rule: min of cap and model max, 0 = unknown/none", () => {
  const c = (n) => ({ schema: {}, defaults: {}, models: [], values: { "relo.models.contextLimit": n } });
  assert.equal(mainApi.effectiveWindow(c(350000), 200000), 200000);
  assert.equal(mainApi.effectiveWindow(c(350000), 1000000), 350000);
  assert.equal(mainApi.effectiveWindow(c(350000), 0), 350000);
  assert.equal(mainApi.effectiveWindow(c(0), 64000), 64000);
  assert.equal(mainApi.effectiveWindow(c(0), 0), 0);
});

test("providers list shows key status, never a key value; remove asks first", () => {
  const cache = cacheFx({ "relo.providers": [{ id: "acme", name: "Acme", kind: "openai", baseURL: "https://a.test/v1", models: [{ id: "m" }] }, { id: "e", name: "E", kind: "anthropic", baseURL: "http://localhost:1", models: [] }] });
  cache.providerKeys = { acme: true, e: false };
  const html = mainApi.settingsPageHtml(cache, "");
  assert.ok(html.includes("Key set") && html.includes("No key"));
  assert.ok(html.includes("1 model") && html.includes("0 models"));
  assert.ok(html.includes('data-prov-remove="acme"') && !html.includes("data-prov-remove-yes"));
  assert.ok(!/type="password"[^>]*value=/.test(html));
});

test("provider form validation: id, reserved, duplicate, URL, model ids", () => {
  const ok = { editing: "", id: "mine", baseURL: "https://x.test/v1", models: [{ id: "a" }] };
  assert.equal(mainApi.providerFormIssue(ok, []), "");
  assert.match(mainApi.providerFormIssue({ ...ok, id: "Bad Id" }, []), /Id/);
  assert.match(mainApi.providerFormIssue({ ...ok, id: "openai" }, []), /reserved/);
  assert.match(mainApi.providerFormIssue(ok, [{ id: "mine" }]), /already exists/);
  assert.equal(mainApi.providerFormIssue({ ...ok, editing: "mine" }, [{ id: "mine" }]), "");
  assert.match(mainApi.providerFormIssue({ ...ok, baseURL: "ftp://x" }, []), /http/);
  assert.match(mainApi.providerFormIssue({ ...ok, baseURL: "https://u:p@x.test" }, []), /credentials/);
  assert.match(mainApi.providerFormIssue({ ...ok, models: [{ id: "a" }, { id: "a" }] }, []), /Duplicate/);
  assert.match(mainApi.providerFormIssue({ ...ok, models: [{ id: "a b" }] }, []), /spaces/);
});

test("provider form output carries only known fields and never an API key", () => {
  const f = { id: "mine", name: " ", kind: "anthropic", baseURL: " https://x.test ", apiKey: "sk-leak", models: [
    { id: "a", name: "", context: "200,000", output: "bad", efforts: "Low, high, low, !!" }, { id: " ", name: "x" }] };
  const p = mainApi.providerFromForm(f);
  assert.deepEqual(p, { id: "mine", name: "mine", kind: "anthropic", baseURL: "https://x.test", models: [{ id: "a", name: "a", context: 200000, reasoning: true, efforts: ["low", "high"] }] });
  assert.ok(!JSON.stringify(p).includes("sk-leak"));
});

test("slugify makes a valid provider id", () => {
  assert.equal(mainApi.slugify("My Gateway!"), "my-gateway");
  assert.equal(mainApi.slugify("  --X  "), "x");
  assert.equal(mainApi.slugify("a".repeat(80)).length, 40);
});

const pickModels = [
  { id: "claude-opus-4", provider: "anthropic", name: "Claude Opus 4", context: 200000, efforts: ["low", "high"] },
  { id: "claude-haiku-4", provider: "anthropic", name: "Claude Haiku 4" },
  { id: "gpt-5", provider: "openai", name: "GPT-5" },
  { id: "acme-big", provider: "acme", name: "Acme Big" },
];

test("model search: every word must match provider/id or name, any order, any case", () => {
  const ids = (q) => mainApi.filterModels(pickModels, q).map((m) => m.id);
  assert.deepEqual(ids(""), pickModels.map((m) => m.id));
  assert.deepEqual(ids("opus"), ["claude-opus-4"]);
  assert.deepEqual(ids("CLAUDE 4"), ["claude-opus-4", "claude-haiku-4"]);
  assert.deepEqual(ids("anthropic/claude-h"), ["claude-haiku-4"]);
  assert.deepEqual(ids("acme gpt"), []);
  assert.deepEqual(ids("  gpt  "), ["gpt-5"]);
  assert.deepEqual(ids("Acme Big"), ["acme-big"]);
});

test("picker rows: providers sorted (models keep catalog order), collapsed ones hide models, search ignores collapse", () => {
  const open = mainApi.pickerRows(pickModels, "", "", "(none)", {});
  assert.deepEqual(open.rows.map((r) => r.kind + ":" + (r.provider || r.ref || "blank")), [
    "opt:blank", "group:acme", "opt:acme/acme-big", "group:anthropic", "opt:anthropic/claude-opus-4", "opt:anthropic/claude-haiku-4", "group:openai", "opt:openai/gpt-5"]);
  const col = mainApi.pickerRows(pickModels, "", "", "(none)", { "p:anthropic": true });
  assert.ok(col.rows.some((r) => r.kind === "group" && r.provider === "anthropic" && r.open === false && r.count === 2));
  assert.ok(!col.rows.some((r) => r.ref && r.ref.startsWith("anthropic/")));
  const found = mainApi.pickerRows(pickModels, "opus", "", "(none)", { "p:anthropic": true });
  assert.deepEqual(found.rows.map((r) => r.key), ["g:anthropic", "o:anthropic/claude-opus-4"]);
  assert.equal(found.rows[0].toggle, false, "headers do not toggle while searching");
  assert.equal(found.matches, 1);
  assert.equal(mainApi.pickerCountText(found, pickModels.length), "1 of 4 models");
  assert.equal(mainApi.pickerCountText(mainApi.pickerRows(pickModels, "zzz", "", "(none)", {}), 4), "No models match");
});

test("picker rows: a saved model missing from the catalog stays visible and selectable", () => {
  const r = mainApi.pickerRows(pickModels, "", "ghost/old-model", "(none)", {});
  assert.ok(r.rows.some((x) => x.ref === "ghost/old-model"));
  const bare = mainApi.pickerRows(pickModels, "", "noslash", "(none)", {});
  assert.ok(bare.rows.some((x) => x.ref === "other/noslash"), "unknown shape goes under other without changing the saved ref");
});

test("picker html: closed shows current value, open has search box, tree roles, escapes ids", () => {
  const closed = mainApi.pickerClosedHtml("mp-x", 'data-pid="mp-x"', "Primary model", "anthropic/claude-opus-4");
  assert.ok(closed.includes('role="combobox"') && closed.includes('aria-expanded="false"') && closed.includes("anthropic/claude-opus-4"));
  const evil = [{ id: '"><img src=x onerror=1>', provider: "p", name: "<b>" }];
  const html = mainApi.pickerOpenHtml("mp-x", 'data-pid="mp-x"', "Primary model", evil, "", "(none)", { id: "mp-x", query: "", active: "" }, {});
  assert.ok(html.includes('class="mpick-q"') && html.includes('role="tree"') && html.includes('role="treeitem"'));
  assert.ok(!html.includes("<img") && !html.includes("<b>"));
  const sel = mainApi.pickerOpenHtml("mp-x", "", "Primary model", pickModels, "openai/gpt-5", "(none)", { id: "mp-x", query: "", active: "" }, {});
  assert.ok(/aria-selected="true"[^>]*data-ref="openai\/gpt-5"/.test(sel) || /data-ref="openai\/gpt-5"/.test(sel));
  assert.ok(sel.includes("aria-activedescendant"));
});

test("settings page renders a searchable picker for model rows and saved collapse state", () => {
  const schema = Object.fromEntries(["relo.routing.mode", "relo.primaryModel", "relo.tier1Model", "relo.tier2Model", "relo.models.contextLimit"].map((k) => [k, { description: k }]));
  const html = mainApi.settingsPageHtml({ schema, defaults: {}, models: pickModels, values: { "relo.routing.mode": "simple", "relo.primaryModel": "openai/gpt-5", "relo.providers": [] } }, "");
  assert.equal((html.match(/class="mpick-btn"/g) || []).length, 3);
  assert.ok(!/<select[^>]*data-key="relo\.(primary|tier1|tier2)Model"/.test(html), "model rows no longer use native selects");
});

test("cleanCollapsed in the webview drops anything but valid keys set to true", () => {
  assert.deepEqual(mainApi.cleanCollapsed({ "g:models": true, "p:acme": true, "p:x": false, bad: true, "p:a b": true }), { "g:models": true, "p:acme": true });
  assert.deepEqual(mainApi.cleanCollapsed(null), {});
  assert.deepEqual(mainApi.cleanCollapsed([1]), {});
});

test("picker keyboard: searching highlights the first model and skips static headers", () => {
  const found = mainApi.pickerRows(pickModels, "claude", "", "(none)", {});
  assert.equal(mainApi.defaultActive(found, ""), "o:anthropic/claude-opus-4");
  assert.deepEqual(mainApi.navKeys(found), ["o:anthropic/claude-opus-4", "o:anthropic/claude-haiku-4"]);
  const browse = mainApi.pickerRows(pickModels, "", "openai/gpt-5", "(none)", {});
  assert.equal(mainApi.defaultActive(browse, "openai/gpt-5"), "o:openai/gpt-5");
  assert.equal(mainApi.defaultActive(mainApi.pickerRows(pickModels, "", "", "(none)", {}), ""), "o:");
  assert.ok(mainApi.navKeys(browse).includes("g:anthropic"), "headers are reachable when browsing so they can be toggled");
  assert.equal(mainApi.defaultActive(mainApi.pickerRows(pickModels, "zzz", "", "(none)", {}), ""), "");
});

// ---- Run panel, status, keyboard, error rendering (candidate UI) ----
const ui = require("./main.js");
const NOW_MS = Date.parse("2026-10-14T12:05:00Z");

test("isRunning: session.running, live agent statuses, streaming; idle false; missing fields tolerated", () => {
  assert.equal(ui.isRunning({ session: { running: true }, agents: [] }), true);
  assert.equal(ui.isRunning({ agents: [{ status: "queued" }] }), true);
  assert.equal(ui.isRunning({ agents: [{ status: "waiting" }] }), true);
  assert.equal(ui.isRunning({ agents: [{ status: "interrupted" }], messages: [] }), false);
  assert.equal(ui.isRunning({ session: { running: false }, agents: [{ status: "completed" }] }), false);
  assert.equal(ui.isRunning({}), false);
  assert.equal(ui.isRunning(null), false);
});

test("fmtElapsed: mm:ss, minutes keep growing, negative clamps to 0:00", () => {
  assert.equal(ui.fmtElapsed(0), "0:00");
  assert.equal(ui.fmtElapsed(9_999), "0:09");
  assert.equal(ui.fmtElapsed(65_000), "1:05");
  assert.equal(ui.fmtElapsed(3_599_000), "59:59");
  assert.equal(ui.fmtElapsed(3_600_000), "60:00");
  assert.equal(ui.fmtElapsed(-5), "0:00");
});

test("run panel: absent when idle; primary activity label, elapsed text node carries data-elapsed-from", () => {
  assert.equal(ui.runPanelHtml({ agents: [{ id: "p", role: "primary", status: "completed" }] }, NOW_MS, {}), "");
  const s = { agents: [{ id: "p", role: "primary", status: "running", activity: "Reasoning", startedAt: NOW_MS - 75_000 }], messages: [] };
  const html = ui.runPanelHtml(s, NOW_MS, {});
  assert.ok(html.includes(">Reasoning<"));
  assert.ok(html.includes(`data-elapsed-from="${NOW_MS - 75_000}">1:15<`));
  assert.ok(html.includes('aria-label="Stop run"'));
  assert.ok(html.includes('role="status" aria-live="polite"'));
});

test("run panel: phase fallback label when activity empty, waiting shows coordinating", () => {
  const h1 = ui.runPanelHtml({ agents: [{ id: "p", role: "primary", status: "running", phase: "tool", activity: "" }] }, NOW_MS, {});
  assert.ok(h1.includes("Running tool"));
  const h2 = ui.runPanelHtml({ agents: [{ id: "p", role: "primary", status: "waiting", activity: "Coordinating sub-agents" }] }, NOW_MS, {});
  assert.ok(h2.includes("Coordinating sub-agents"));
});

test("run panel: each sub-agent status maps to its card kind and icon", () => {
  assert.deepEqual(ui.cardKind("running"), { kind: "live", icon: "codicon-loading codicon-modifier-spin" });
  assert.equal(ui.cardKind("queued").icon, "codicon-clock");
  assert.equal(ui.cardKind("waiting").icon, "codicon-shield");
  assert.equal(ui.cardKind("completed").icon, "codicon-check");
  assert.equal(ui.cardKind("failed").icon, "codicon-error");
  assert.equal(ui.cardKind("cancelled").icon, "codicon-debug-stop");
});

test("run panel: only agents of the current run (created after last user message), newest first, +N earlier cap", () => {
  const t0 = Date.parse("2026-10-14T12:00:00Z");
  const msgs = [{ role: "user", createdAt: new Date(t0 + 60_000).toISOString() }];
  const old = { id: "old", role: "worker", title: "OLD", status: "completed", createdAt: t0 };
  const agents = [{ id: "p", role: "primary", status: "waiting", activity: "Coordinating sub-agents" }, old];
  for (let i = 0; i < 7; i++) agents.push({ id: "w" + i, role: "worker", title: "W" + i, status: "running", createdAt: t0 + 120_000 + i * 1000 });
  const s = { agents, messages: msgs };
  const html = ui.runPanelHtml(s, NOW_MS, {});
  assert.ok(!html.includes(">OLD<"), "previous run agent hidden");
  assert.ok(html.includes("Sub-agents (7)"));
  assert.ok(html.includes("+2 earlier"));
  assert.equal((html.match(/class="run-card /g) || []).length, 5);
  assert.ok(html.indexOf(">W6<") < html.indexOf(">W5<"), "newest first");
  const expanded = ui.runPanelHtml(s, NOW_MS, { showAll: true });
  assert.equal((expanded.match(/class="run-card /g) || []).length, 7);
  assert.ok(expanded.includes("Show fewer"));
  assert.deepEqual(ui.runAgents(s).map((a) => a.id), ["w6", "w5", "w4", "w3", "w2", "w1", "w0"]);
});

test("run panel: finished card shows total duration and tool count; running shows live elapsed", () => {
  const t0 = NOW_MS - 600_000;
  const s = { agents: [
    { id: "p", role: "primary", status: "waiting", activity: "x" },
    { id: "d", role: "worker", title: "Done one", status: "completed", startedAt: t0, endedAt: t0 + 42_000, tools: 3, model: "openai/gpt-5" },
    { id: "r", role: "worker", title: "Live one", status: "running", startedAt: NOW_MS - 5_000, tools: 1, activity: "grep" },
  ], messages: [] };
  const html = ui.runPanelHtml(s, NOW_MS, { cardOpen: { d: true } });
  assert.ok(html.includes(">0:42<"));
  assert.ok(html.includes("3 tools"));
  assert.ok(html.includes("1 tool<"));
  assert.ok(html.includes('data-elapsed-from="' + (NOW_MS - 5_000) + '"'));
  assert.ok(html.includes("gpt-5"));
  assert.ok(html.includes('aria-expanded="true"'));
  assert.ok(html.includes("grep"));
});

test("run panel: failed card shows escaped error text in a truncated live line", () => {
  const s = { agents: [
    { id: "p", role: "primary", status: "waiting", activity: "x" },
    { id: "f", role: "worker", title: "Broke", status: "failed", activity: "<b>401</b> bad key" },
  ], messages: [] };
  const html = ui.runPanelHtml(s, NOW_MS, {});
  assert.ok(html.includes("&lt;b&gt;401&lt;/b&gt; bad key"));
  assert.ok(!html.includes("<b>401</b>"));
  assert.ok(html.includes("run-err"));
});

test("error role renders an error callout with the provider text, not an assistant bubble", () => {
  const h = ui.msgHtml({ id: "e", role: "error", text: "429 rate limit exceeded" });
  assert.ok(h.includes('class="msg error"'));
  assert.ok(h.includes("codicon-error"));
  assert.ok(h.includes("429 rate limit exceeded"));
  assert.ok(!h.includes("msg-role"));
});

test("assistant label no longer shows agent id; streaming bubble has a caret", () => {
  const h = ui.msgHtml({ id: "a", role: "assistant", agentId: "agent_123", text: "hi", streaming: true });
  assert.ok(h.includes(">Assistant<"));
  assert.ok(!h.includes("agent_123"));
  assert.ok(h.includes('class="stream"'));
});

test("agent messages are distinct cards with a role header; queued user messages say so", () => {
  const h = ui.msgHtml({ id: "a", role: "assistant", agentId: "agent_123", text: "hi" });
  assert.ok(h.includes('class="msg agent"'));
  assert.ok(h.includes("codicon-hubot"));
  assert.ok(h.includes('class="msg-who"'));
  const user = ui.msgHtml({ id: "u", role: "user", text: "do it", queued: true });
  assert.ok(user.includes('class="msg user"'));
  assert.ok(user.includes("msg-queued"));
  const delivered = ui.msgHtml({ id: "u", role: "user", text: "do it" });
  assert.ok(!delivered.includes("msg-queued"));
});

test("Enter sends while running and idle; Shift+Enter and IME composition ignored", () => {
  assert.equal(ui.enterAction({ key: "Enter", shiftKey: false }), "send");
  assert.equal(ui.enterAction({ key: "Enter", shiftKey: true }), "ignore");
  assert.equal(ui.enterAction({ key: "a" }), "ignore");
  assert.equal(ui.enterAction({ key: "Enter", shiftKey: false, isComposing: true }), "ignore");
});

test("running composer: Send and a separate Stop both render; idle has no Stop", () => {
  const running = ui.chatHtml({ agents: [{ id: "p", role: "primary", status: "running" }], messages: [] });
  assert.ok(running.includes('id="send"'));
  assert.ok(running.includes('id="stop"'));
  assert.ok(running.includes('aria-label="Stop run"'));
  const idle = ui.chatHtml({ agents: [], messages: [] });
  assert.ok(idle.includes('id="send"'));
  assert.ok(!idle.includes('id="stop"'));
});

test("scroll after render: sticks to bottom when near bottom or a new message arrived, else keeps position", () => {
  assert.equal(ui.scrollAfterRender({ nearBottom: true, scrollTop: 0, msgCount: 3 }, 3, 900), 900);
  assert.equal(ui.scrollAfterRender({ nearBottom: false, scrollTop: 120, msgCount: 3 }, 4, 900), 900);
  assert.equal(ui.scrollAfterRender({ nearBottom: false, scrollTop: 120, msgCount: 3 }, 3, 900), 120);
  assert.equal(ui.scrollAfterRender(null, 3, 900), 900);
});

test("phase labels cover the backend phase protocol", () => {
  for (const p of ["context", "model", "reasoning", "writing", "tool", "verifying", "approval", "waiting", "compacting", "retry"]) {
    assert.notEqual(ui.phaseLabel(p), "", p);
  }
  assert.equal(ui.phaseLabel("reasoning"), "Reasoning");
  assert.equal(ui.phaseLabel(""), "Working");
});

test("changes drawer: letter follows kind, decided changes are dimmed and locked, bulk buttons use the bulk commands", () => {
  const html = mainApi.drawerHtml("changes", {
    changes: [
      { id: "c1", path: "a.ts", status: "proposed", kind: "modified", added: 3, deleted: 1 },
      { id: "c2", path: "b.ts", status: "proposed", kind: "added", added: 9 },
      { id: "c3", path: "c.ts", status: "proposed", kind: "deleted", deleted: 4 },
      { id: "c4", path: "d.ts", status: "accepted", kind: "modified" },
    ],
  });
  assert.ok(html.includes('class="letter M"') && html.includes('class="letter A"') && html.includes('class="letter D"'));
  assert.ok(html.includes("+3 -1"));
  assert.equal((html.match(/disabled/g) || []).length, 2, "only the accepted change loses its buttons");
  assert.ok(html.includes('data-cmd="relo.changes.acceptAll"') && html.includes("Accept all (3)"));
  assert.ok(!html.includes("data-command-all"));
  const one = mainApi.drawerHtml("changes", { changes: [{ id: "c1", path: "a.ts", status: "proposed", kind: "modified" }] });
  assert.ok(!one.includes("acceptAll"), "a single change needs no bulk buttons");
});

test("changes drawer says what the changes are measured against, only while something waits", () => {
  const waiting = mainApi.drawerHtml("changes", { changes: [{ id: "c1", path: "a.ts", status: "proposed", kind: "modified" }] });
  assert.ok(waiting.includes("Compared with your files when the agent started") && waiting.includes("until you accept it"));
  const done = mainApi.drawerHtml("changes", { changes: [{ id: "c1", path: "a.ts", status: "accepted", kind: "modified" }] });
  assert.ok(!done.includes("Compared with your files"), "nothing is waiting, so there is nothing to explain");
});

test("changes drawer: auto-apply defaults off and repository selection stays visible without proposals", () => {
  const off = mainApi.drawerHtml("changes", { changes: [] });
  assert.match(off, /type="checkbox" data-auto-apply\/>/);
  assert.ok(off.includes('data-cmd="relo.selectRepository"'));
  assert.ok(off.includes("No changes"));
  assert.ok(off.includes("Unstaged edits only. No commit or push."));
  const on = mainApi.drawerHtml("changes", { changes: [] }, {
    values: { "relo.changes.autoApply": true },
    repository: { root: "/work/<repo>", branch: "feature/<current>" },
  });
  assert.match(on, /data-auto-apply checked/);
  assert.ok(on.includes("/work/&lt;repo&gt;"));
  assert.ok(on.includes("feature/&lt;current&gt;"));
  assert.ok(!on.includes("feature/<current>"));
});

test("settings: Changes group has the auto-apply toggle and the repository row", () => {
  const cache = cacheFx();
  cache.schema = { ...schemaFx, "relo.changes.autoApply": { type: "boolean", description: "Apply proposed changes automatically" } };
  cache.values["relo.changes.autoApply"] = true;
  cache.values["relo.repository"] = "/work/<repo>";
  const html = mainApi.settingsPageHtml(cache, "");
  assert.ok(html.includes('data-group="changes"'));
  assert.match(html, /data-key="relo.changes.autoApply"[^>]*checked/);
  assert.ok(html.includes('data-cmd="relo.selectRepository"'));
  assert.ok(html.includes("/work/&lt;repo&gt;"));
  assert.ok(html.includes("Folder Relo edits"));
  const empty = mainApi.settingsPageHtml({ ...cache, values: { "relo.routing.mode": "simple", "relo.providers": [] } }, "");
  assert.ok(empty.includes("First workspace folder"));
});

test("changes drawer: conflict badge, resolving state, superseded changes hidden", () => {
  const html = mainApi.drawerHtml("changes", {
    changes: [
      { id: "c1", path: "a.ts", status: "proposed", kind: "modified", conflict: "overlap", resolving: true },
      { id: "c2", path: "b.ts", status: "proposed", kind: "modified", superseded: true },
    ],
  });
  assert.ok(html.includes("Resolving 1 conflict via head agent"));
  assert.ok(html.includes('class="badge" title="overlap"'));
  assert.ok(!html.includes("b.ts"), "superseded change leaves the proposed list");
  const decided = mainApi.drawerHtml("changes", { changes: [{ id: "c1", path: "a.ts", status: "accepted", kind: "modified", conflict: "overlap", resolving: true }] });
  assert.ok(!decided.includes('class="badge" title="overlap"'), "decided change shows no conflict badge");
  assert.ok(!decided.includes("Resolving"));
  const none = mainApi.drawerHtml("changes", { changes: [{ id: "c1", path: "a.ts", status: "proposed", kind: "modified" }] });
  assert.ok(!none.includes("Resolving"));
  assert.ok(!none.includes('class="badge"'));
});

test("chips count only changes still waiting for a decision", () => {
  const c = mainApi.chipCounts({ changes: [{ status: "proposed" }, { status: "accepted" }, { status: "rejected" }, { status: "applied" }] });
  assert.equal(c.changes.label, "2");
  assert.equal(mainApi.isDecided({ status: "Accepted" }), true);
  assert.equal(mainApi.isDecided({ status: "proposed" }), false);
});

test("relTime: coarse ages, empty for junk", () => {
  const now = Date.parse("2026-10-14T12:00:00Z");
  assert.equal(mainApi.relTime("2026-10-14T11:59:50Z", now), "just now");
  assert.equal(mainApi.relTime("2026-10-14T11:30:00Z", now), "30 min ago");
  assert.equal(mainApi.relTime("2026-10-14T09:00:00Z", now), "3 h ago");
  assert.equal(mainApi.relTime("2026-10-12T12:00:00Z", now), "2 d ago");
  assert.equal(mainApi.relTime("nonsense", now), "");
});

test("history page: loading, empty, error and rows with open/delete hooks and escaped titles", () => {
  const now = Date.parse("2026-10-14T12:00:00Z");
  assert.ok(mainApi.historyHtml({ sessions: null, error: "" }, now).includes("Loading"));
  assert.ok(mainApi.historyHtml({ sessions: [], error: "" }, now).includes("No earlier sessions"));
  assert.ok(mainApi.historyHtml({ sessions: [], error: "db locked" }, now).includes("db locked"));
  const html = mainApi.historyHtml({
    error: "",
    sessions: [
      { id: "sess_1", title: "<b>Fix login</b>", updatedAt: "2026-10-14T11:30:00Z", messages: 4, pendingChanges: 2, current: false },
      { id: "sess_2", title: "Refactor", updatedAt: "2026-10-14T11:59:59Z", messages: 1, pendingChanges: 0, current: true },
    ],
  }, now);
  assert.ok(html.includes('data-session-open="sess_1"') && html.includes('data-session-delete="sess_2"'));
  assert.ok(html.includes("&lt;b&gt;Fix login") && !html.includes("<b>Fix login"));
  assert.ok(html.includes("30 min ago · 4 messages") && html.includes("open now"));
  assert.ok(html.includes('class="badge" title="Changes waiting for a decision">2'));
  assert.ok(html.includes('data-cmd="relo.newSession"'));
});

test("toolbar has a history button that flips back to chat while the page is open", () => {
  const idle = mainApi.toolbarHtml({ session: { objective: "x" } });
  assert.ok(idle.includes('data-page="history"') && idle.includes('data-cmd="relo.newSession"'));
});

test("markdown: headings, emphasis, links and rules render; HTML stays escaped", () => {
  const out = md("## Title\n\nSome **bold**, *italic*, _also italic_, ~~gone~~ and `a*b*c`.\n\n---\n\nSee [docs](https://example.com/a_b_c?x=1&y=2) or https://example.org/path.");
  assert.ok(out.includes("<h2>Title</h2>"));
  assert.ok(out.includes("<strong>bold</strong>") && out.includes("<em>italic</em>") && out.includes("<em>also italic</em>") && out.includes("<del>gone</del>"));
  assert.ok(out.includes("<code>a*b*c</code>"), "emphasis must not touch code spans");
  assert.ok(out.includes("<hr/>"));
  assert.ok(out.includes('<a href="https://example.com/a_b_c?x=1&amp;y=2"'), "underscores in a URL are not emphasis");
  assert.ok(out.includes('<a href="https://example.org/path"') && out.includes("</a>."), "trailing period stays outside the link");
  assert.ok(!md("<script>x</script> **a**").includes("<script>"));
});

test("markdown: bullet, numbered and nested lists", () => {
  const out = md("- one\n- two\n  - nested a\n  - nested b\n- three\n\n1. first\n2. second\n\n3. third after a gap");
  assert.equal((out.match(/<ul>/g) || []).length, 2, "outer and nested bullet lists");
  assert.ok(out.includes("<li>two<ul><li>nested a</li><li>nested b</li></ul></li>"));
  assert.ok(out.includes("<ol><li>first</li><li>second</li><li>third after a gap</li></ol>"));
  assert.ok(md("3. resumes here\n4. next").includes('<ol start="3">'));
  assert.ok(md("- [ ] todo\n- [x] done").includes("☐ todo") && md("- [x] done").includes("☑ done"));
});

test("markdown: pipe tables with alignment, escaped pipes and inline formatting", () => {
  const out = md("| Name | Qty | Note |\n|:--|--:|:-:|\n| **a** | 1 | x \\| y |\n| `b` | 22 |\n\nafter");
  assert.ok(out.includes('<div class="md-table"><table>'));
  assert.ok(out.includes('<th class="al-l">Name</th>') && out.includes('<th class="al-r">Qty</th>') && out.includes('<th class="al-c">Note</th>'));
  assert.ok(out.includes('<td class="al-l"><strong>a</strong></td>') && out.includes('<td class="al-c">x | y</td>'));
  assert.ok(out.includes('<td class="al-c"></td>'), "short rows are padded");
  assert.ok(out.includes("<p>after</p>"));
  assert.ok(!md("just a | pipe in prose").includes("<table"));
});

test("markdown: blockquote, paragraph breaks and code fences interleave", () => {
  const out = md("> quoted **line**\n> more\n\nPara one\nstill para one\n\n```js\nconst a = 1;\n```\ntail");
  assert.ok(out.includes("<blockquote><p>quoted <strong>line</strong><br/>more</p></blockquote>"));
  assert.ok(out.includes("<p>Para one<br/>still para one</p>"));
  assert.ok(out.includes('<span>js</span>') && out.includes("<p>tail</p>"));
});

test("markdown: an unfinished fence while streaming renders as code, not as prose", () => {
  const out = md("Here:\n```ts\nconst a = <b>1</b>;");
  assert.ok(out.includes('class="code-block"'));
  assert.ok(out.includes("const a = &lt;b&gt;1&lt;/b&gt;;"));
});

test("markdown: a real assistant reply with a table and bullets has no stray markup characters", () => {
  const out = md("Suggestions:\n\n| A | B |\n|---|---|\n| **x** | y |\n\n- **Option:** `lite`\n- plain");
  assert.ok(!/\|---/.test(out) && !out.includes("**") && !out.includes("\n- "));
});

test("markdown: a fence indented inside a list item loses that indent", () => {
  const out = md("1. Step\n   ```sh\n   npm test\n     --watch\n   ```\n2. Next");
  const code = out.match(/<pre><code>([\s\S]*?)<\/code><\/pre>/)[1];
  assert.equal(code, "npm test\n  --watch");
  assert.ok(out.includes('<ol start="2"><li>Next</li></ol>'));
});
