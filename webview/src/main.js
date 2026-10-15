/* Relo webview — vanilla JS so the extension host stays thin. */
(function () {
  function esc(s) {
    return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  }

  // Single token formatter used everywhere: 950, 12.3k, 1.2M.
  function fmtTokens(n) {
    const v = Math.max(0, Number(n) || 0);
    if (v < 999.5) return String(Math.round(v));
    if (v < 999950) return (v / 1000).toFixed(1) + "k";
    return (v / 1000000).toFixed(1) + "M";
  }

  function plural(n, word) {
    return n + " " + word + (n === 1 ? "" : "s");
  }

  function submit(box, postMessage) {
    const text = box.value.trim();
    if (!text) return;
    postMessage({ type: "submit", text });
    box.value = "";
  }

  // Attribute-safe escape; keeps CR so copy payload round-trips exactly.
  function attr(s) {
    return esc(s).replace(/\r/g, "&#13;");
  }

  // Inline markdown. Code spans and links are lifted out first so emphasis never touches their
  // contents (a URL with underscores, a snippet with asterisks). Everything is escaped before any
  // tag is produced, and only http(s)/mailto links get an href.
  function inline(s) {
    const stash = [];
    const keep = (html) => "\u0000" + (stash.push(html) - 1) + "\u0001";
    let t = String(s ?? "").replace(/[\u0000\u0001]/g, "");
    t = t.replace(/`([^`\n]+)`/g, (_, c) => keep(`<code>${esc(c)}</code>`));
    t = esc(t);
    t = t.replace(/\[([^\]\n]+)\]\(([^)\s]+)(?:\s+&quot;[^\n]*?&quot;)?\)/g, (m, label, url) => {
      if (!/^(https?:\/\/|mailto:)/i.test(url)) return m;
      return keep(`<a href="${url}" title="${url}">${emphasis(label)}</a>`);
    });
    t = t.replace(/(^|[\s(])(https?:\/\/[^\s<]*[^\s<.,;:!?)\]'])/g, (m, pre, url) => pre + keep(`<a href="${url}" title="${url}">${url}</a>`));
    t = emphasis(t).replace(/\n/g, "<br/>");
    return t.replace(/\u0000(\d+)\u0001/g, (_, i) => stash[Number(i)]);
  }

  function emphasis(t) {
    return t
      .replace(/\*\*([^\s*](?:[^*\n]*[^\s*])?)\*\*/g, "<strong>$1</strong>")
      .replace(/(^|[^\w])__([^\s_](?:[^_\n]*[^\s_])?)__(?!\w)/g, "$1<strong>$2</strong>")
      .replace(/(^|[^\w*])\*([^\s*](?:[^*\n]*[^\s*])?)\*(?![\w*])/g, "$1<em>$2</em>")
      .replace(/(^|[^\w])_([^\s_](?:[^_\n]*[^\s_])?)_(?!\w)/g, "$1<em>$2</em>")
      .replace(/~~([^\s~](?:[^~\n]*[^\s~])?)~~/g, "<del>$1</del>");
  }

  function codeBlock(lang, code) {
    const copyIcon = `<span class="codicon codicon-copy" aria-hidden="true"></span>`;
    return `<div class="code-block"><div class="code-head"><span>${esc(lang || "code")}</span><button type="button" class="code-copy" data-code="${attr(code)}" aria-label="Copy code" title="Copy code">${copyIcon}</button></div><pre><code>${esc(code)}</code></pre></div>`;
  }

  const FENCE_RE = /^\s{0,3}(`{3,}|~{3,})\s*([^`\n]*)$/;
  const HEADING_RE = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
  const RULE_RE = /^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/;
  const ITEM_RE = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
  const QUOTE_RE = /^\s{0,3}>\s?(.*)$/;
  const TABLE_SEP_RE = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;

  // Splits a table row on pipes that are neither escaped nor inside a code span.
  function tableCells(line) {
    const cells = [];
    let cur = "";
    let tick = false;
    for (let i = 0; i < line.length; i++) {
      const c = line[i];
      if (c === "\\" && line[i + 1] === "|") { cur += "|"; i++; continue; }
      if (c === "`") tick = !tick;
      if (c === "|" && !tick) { cells.push(cur); cur = ""; continue; }
      cur += c;
    }
    cells.push(cur);
    if (cells.length && cells[0].trim() === "") cells.shift();
    if (cells.length && cells[cells.length - 1].trim() === "") cells.pop();
    return cells.map((c) => c.trim());
  }

  function startsTable(lines, i) {
    return lines[i].includes("|") && i + 1 < lines.length && lines[i + 1].includes("-") && TABLE_SEP_RE.test(lines[i + 1]) && lines[i + 1].includes("|");
  }

  function tableHtml(head, aligns, rows) {
    const cell = (tag, text, n) => `<${tag}${aligns[n] ? ` class="al-${aligns[n]}"` : ""}>${inline(text)}</${tag}>`;
    const th = head.map((h, n) => cell("th", h, n)).join("");
    const body = rows.map((r) => `<tr>${head.map((_, n) => cell("td", r[n] || "", n)).join("")}</tr>`).join("");
    return `<div class="md-table"><table><thead><tr>${th}</tr></thead><tbody>${body}</tbody></table></div>`;
  }

  // Items arrive flat with their indent; nesting is rebuilt from the indent.
  function listHtml(items) {
    let html = "";
    const stack = [];
    const close = () => { html += `</li></${stack.pop().tag}>`; };
    for (const it of items) {
      const tag = it.ordered ? "ol" : "ul";
      while (stack.length && it.indent < stack[stack.length - 1].indent) close();
      const top = stack[stack.length - 1];
      let open = true;
      if (top && it.indent >= top.indent + 2) {
        open = true;
      } else if (top) {
        if (top.tag === tag) { html += "</li>"; open = false; } else { close(); }
      }
      if (open) {
        html += `<${tag}${tag === "ol" && it.num !== 1 ? ` start="${it.num}"` : ""}>`;
        stack.push({ indent: it.indent, tag });
      }
      const task = /^\[([ xX])\]\s+/.exec(it.text);
      html += "<li>" + (task ? (task[1] === " " ? "☐ " : "☑ ") : "") + inline(task ? it.text.slice(task[0].length) : it.text);
    }
    while (stack.length) close();
    return html;
  }

  const blockStart = (lines, i) => FENCE_RE.test(lines[i]) || HEADING_RE.test(lines[i]) || RULE_RE.test(lines[i]) || ITEM_RE.test(lines[i]) || QUOTE_RE.test(lines[i]) || startsTable(lines, i);

  function blocks(src) {
    const lines = src.replace(/\r\n?/g, "\n").split("\n");
    let out = "";
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (!line.trim()) { i++; continue; }

      let m = FENCE_RE.exec(line);
      if (m) {
        // An unterminated fence runs to the end, so a reply that is still streaming shows as code.
        const body = [];
        const strip = new RegExp("^ {0," + /^ */.exec(line)[0].length + "}");
        i++;
        while (i < lines.length && !(lines[i].trim().startsWith(m[1][0].repeat(m[1].length)) && /^\s*[`~]+\s*$/.test(lines[i]))) body.push(lines[i++].replace(strip, ""));
        i++;
        out += codeBlock(m[2].trim().split(/\s+/)[0], body.join("\n"));
        continue;
      }
      if ((m = HEADING_RE.exec(line))) {
        out += `<h${m[1].length}>${inline(m[2])}</h${m[1].length}>`;
        i++;
        continue;
      }
      if (RULE_RE.test(line)) { out += "<hr/>"; i++; continue; }
      if (startsTable(lines, i)) {
        const head = tableCells(line);
        const aligns = tableCells(lines[i + 1]).map((c) => (c.startsWith(":") && c.endsWith(":") ? "c" : c.endsWith(":") ? "r" : c.startsWith(":") ? "l" : ""));
        const rows = [];
        i += 2;
        while (i < lines.length && lines[i].trim() && lines[i].includes("|")) rows.push(tableCells(lines[i++]));
        out += tableHtml(head, aligns, rows);
        continue;
      }
      if (QUOTE_RE.test(line)) {
        const body = [];
        while (i < lines.length && QUOTE_RE.test(lines[i])) body.push(QUOTE_RE.exec(lines[i++])[1]);
        out += `<blockquote>${blocks(body.join("\n"))}</blockquote>`;
        continue;
      }
      if (ITEM_RE.test(line)) {
        const items = [];
        while (i < lines.length) {
          const im = ITEM_RE.exec(lines[i]);
          if (im && !RULE_RE.test(lines[i])) {
            items.push({ indent: im[1].replace(/\t/g, "    ").length, ordered: /\d/.test(im[2]), num: parseInt(im[2], 10) || 1, text: im[3] });
            i++;
          } else if (!lines[i].trim()) {
            // Blank lines inside a list are fine as long as another item follows.
            let j = i;
            while (j < lines.length && !lines[j].trim()) j++;
            if (j < lines.length && ITEM_RE.test(lines[j]) && !RULE_RE.test(lines[j])) i = j; else break;
          } else if (/^\s{2,}\S/.test(lines[i]) && !FENCE_RE.test(lines[i]) && items.length) {
            items[items.length - 1].text += "\n" + lines[i].trim();
            i++;
          } else {
            break;
          }
        }
        out += listHtml(items);
        continue;
      }
      const para = [line];
      i++;
      while (i < lines.length && lines[i].trim() && !blockStart(lines, i)) para.push(lines[i++]);
      out += `<p>${inline(para.join("\n"))}</p>`;
    }
    return out;
  }

  function md(s) {
    return blocks(String(s ?? ""));
  }

  function handleKeyDown(e, box, postMessage) {
    if (!e || e.key !== "Enter" || e.shiftKey) return false;
    if (e.preventDefault) e.preventDefault();
    submit(box, postMessage);
    return true;
  }

  const LIVE_STATUSES = ["queued", "starting", "running", "waiting"];

  function isRunning(state) {
    const s = state || {};
    if (s.session && s.session.running) return true;
    if ((s.messages || []).some((m) => m.streaming)) return true;
    return (s.agents || []).some((a) => LIVE_STATUSES.includes(a.status));
  }

  // Enter in the composer while a run is live must not send or cancel; Stop is the button only.
  function enterAction(e, running) {
    if (!e || e.key !== "Enter" || e.shiftKey || e.isComposing) return "ignore";
    return running ? "noop" : "send";
  }

  function toMs(v) {
    if (typeof v === "number") return v;
    if (typeof v === "string") return Date.parse(v) || 0;
    return 0;
  }

  // mm:ss from a millisecond span; minutes keep growing past 59.
  function fmtElapsed(ms) {
    const s = Math.floor(Math.max(0, Number(ms) || 0) / 1000);
    const m = Math.floor(s / 60);
    return m + ":" + String(s % 60).padStart(2, "0");
  }

  const PHASE_LABELS = {
    context: "Loading context", model: "Waiting for model", reasoning: "Reasoning", writing: "Writing reply",
    tool: "Running tool", verifying: "Verifying", approval: "Waiting for approval", waiting: "Coordinating sub-agents",
    compacting: "Compacting context", retry: "Retrying",
  };

  function phaseLabel(phase) {
    return PHASE_LABELS[phase] || "Working";
  }

  // Non-primary agents of the current run: created at or after the last user message, newest first.
  function runAgents(s) {
    const msgs = s.messages || [];
    let since = 0;
    msgs.forEach((m) => { if (m.role === "user") since = Math.max(since, toMs(m.createdAt)); });
    return (s.agents || [])
      .filter((a) => a.role !== "primary" && (a.createdAt == null || !since || toMs(a.createdAt) >= since))
      .sort((a, b) => toMs(b.createdAt) - toMs(a.createdAt));
  }

  // Pinned panel cards: one kind per status, each with an icon and a color class.
  function cardKind(status) {
    switch (status) {
      case "running": case "starting": return { kind: "live", icon: "codicon-loading codicon-modifier-spin" };
      case "waiting": case "blocked": return { kind: "wait", icon: "codicon-shield" };
      case "completed": return { kind: "done", icon: "codicon-check" };
      case "failed": return { kind: "fail", icon: "codicon-error" };
      case "cancelled": case "interrupted": return { kind: "stop", icon: "codicon-debug-stop" };
      default: return { kind: "queued", icon: "codicon-clock" };
    }
  }

  const RUN_CAP = 5;

  // Pinned run panel. Live timers are text nodes with data-elapsed-from (start ms); the ticker updates them in place.
  function runPanelHtml(s, now, ui) {
    const agents = (s || {}).agents || [];
    if (!isRunning(s)) return "";
    const u = ui || {};
    const primary = agents.find((a) => a.role === "primary" && LIVE_STATUSES.includes(a.status)) || agents.find((a) => a.role === "primary") || {};
    const label = primary.activity || phaseLabel(primary.phase || (primary.status === "waiting" ? "waiting" : "model"));
    const started = toMs(primary.startedAt);
    const time = started ? `<span class="run-time" data-elapsed-from="${started}">${fmtElapsed((now || Date.now()) - started)}</span>` : "";
    const list = runAgents(s);
    const shown = u.showAll ? list : list.slice(0, RUN_CAP);
    const more = list.length > RUN_CAP
      ? `<button type="button" class="run-more" data-run-more aria-expanded="${u.showAll ? "true" : "false"}">${u.showAll ? "Show fewer" : "+" + (list.length - RUN_CAP) + " earlier"}</button>`
      : "";
    const cards = shown.map((a) => agentCardHtml(a, now || Date.now(), u.cardOpen || {})).join("");
    const listOpen = u.listOpen !== false;
    const head = list.length
      ? `<button type="button" class="run-list-toggle" data-run-list aria-expanded="${listOpen ? "true" : "false"}"><span class="codicon ${listOpen ? "codicon-chevron-down" : "codicon-chevron-right"}" aria-hidden="true"></span>Sub-agents (${list.length})</button>`
      : "";
    return `<section class="run" aria-label="Run status">
      <div class="run-status">
        <span class="codicon codicon-loading codicon-modifier-spin run-spin" aria-hidden="true"></span>
        <span class="run-dot" aria-hidden="true"></span>
        <span class="run-label" role="status" aria-live="polite">${esc(label)}</span>
        ${time}
        <button type="button" class="run-stop" data-run-stop aria-label="Stop run" title="Stop run"><span class="codicon codicon-debug-stop" aria-hidden="true"></span>Stop</button>
      </div>
      ${head}
      ${list.length && listOpen ? `<ul class="run-cards" role="list">${cards}</ul>${more}` : ""}
    </section>`;
  }

  function agentCardHtml(a, now, cardOpen) {
    const { kind, icon } = cardKind(a.status);
    const live = a.status === "failed" || a.status === "cancelled" ? (a.activity || a.status) : (a.activity || (a.phase ? phaseLabel(a.phase) : a.status));
    const started = toMs(a.startedAt);
    const ended = toMs(a.endedAt);
    let time = "";
    if (kind === "live" && started) time = `<span class="run-card-time" data-elapsed-from="${started}">${fmtElapsed(now - started)}</span>`;
    else if (ended && started) time = `<span class="run-card-time">${fmtElapsed(ended - started)}</span>`;
    const tools = a.tools > 0 ? `<span class="run-card-tools">${esc(plural(a.tools, "tool"))}</span>` : "";
    const model = a.model ? `<span class="run-card-model">${esc(String(a.model).split("/").pop())}</span>` : "";
    const open = !!cardOpen[a.id];
    const detail = [a.model, ctxLabel(a) ? "context " + ctxLabel(a) : "", a.usedTokens ? fmtTokens(a.usedTokens) + " tokens used" : "", (a.files || []).length ? plural(a.files.length, "file") : ""].filter(Boolean).join(" · ");
    return `<li class="run-card ${kind}" data-key="rcl-${attr(a.id)}">
      <button type="button" class="run-card-head" aria-expanded="${open ? "true" : "false"}" aria-controls="rc-${attr(a.id)}" data-run-card="${attr(a.id)}" title="${attr(live)}">
        <span class="codicon ${icon} run-ico" aria-hidden="true"></span>
        <span class="run-card-main">
          <span class="run-card-top"><span class="run-card-title">${esc(a.title || a.role || a.id)}</span><span class="run-role">${esc(titleCase(a.role))}</span></span>
          <span class="run-card-live${kind === "fail" ? " run-err" : ""}">${esc(live)}</span>
        </span>
        <span class="run-card-meta">${time}${tools}${model}</span>
      </button>
      <div class="run-card-body" id="rc-${attr(a.id)}"${open ? "" : " hidden"}>${esc(detail || "No details yet")}</div>
    </li>`;
  }

  // Keeps the composer draft/selection and the messages scroll position across a full re-render.
  let renderedMsgCount = 0; // message count of the DOM currently on screen
  function captureView() {
    const box = document.getElementById("box");
    const m = document.querySelector(".messages");
    const nearBottom = m ? m.scrollHeight - m.scrollTop - m.clientHeight <= 48 : true;
    return {
      draft: box ? box.value : null,
      selStart: box ? box.selectionStart : 0,
      selEnd: box ? box.selectionEnd : 0,
      focused: !!box && document.activeElement === box,
      scrollTop: m ? m.scrollTop : 0,
      nearBottom,
      msgCount: renderedMsgCount,
    };
  }

  // Where .messages should scroll after render: bottom when the user was near it or just sent, else keep position.
  function scrollAfterRender(prev, newCount, scrollHeight) {
    if (!prev) return scrollHeight;
    if (prev.nearBottom || newCount > prev.msgCount) return scrollHeight;
    return prev.scrollTop;
  }

  // Shared state; declared before the test exports so pure renderers can run under node.
  let state = { backend: { ready: false, models: [] }, agents: [], tasks: [], messages: [], changes: [], verification: [], logs: [] };
  // Run panel UI state survives re-renders: sub-agent list toggle, "+N earlier", per-card details.
  const runUi = { listOpen: true, showAll: false, cardOpen: {} };

  let effOpen = false;
  const checkOpen = {}; // verification result index -> output shown
  let composerDraft = ""; // text typed in the composer, kept while another page is showing
  // Condensed main view: at most one drawer open; "settings" replaces the chat page.
  let openDrawerName = "";
  let page = "chat";
  let menuOpen = false;
  let settingsCache = null; // { values, defaults, schema, models } from extension
  let settingsQuery = "";
  // Collapsed sections, "g:<settings group>" or "p:<model provider>" -> true. The extension keeps the master
  // copy in globalState; the webview adopts it once (first settings message) and owns it afterwards.
  let uiCollapsed = {};
  let uiCollapsedLoaded = false;
  let openPicker = null; // { id, query, active } for the one open model picker
  let rendering = false;
  let settingsScroll = 0;
  // Provider form draft. The API key lives only in providerKeyDraft: never persisted, never rendered into an attribute.
  let providerForm = null;
  let providerKeyDraft = "";
  let providerRemoveConfirm = "";
  let providerShowAll = false;
  const pending = {};

  function withState(s, fn) {
    const prev = state;
    state = s || {};
    try { return fn(); } finally { state = prev; }
  }

  // Drawer chip definitions. Count is the number shown in the strip.
  function chipCounts(s) {
    const agents = s.agents || [];
    const running = agents.filter((a) => a.status === "running" || a.status === "starting").length;
    const tasks = s.tasks || [];
    const done = tasks.filter((t) => t.status === "completed").length;
    const changes = (s.changes || []).filter((c) => !isDecided(c)).length;
    const checks = s.verification || [];
    const passed = checks.filter((v) => v.status === "passed" || v.status === "pass").length;
    const pct = contextPct(s);
    return {
      agents: { n: agents.length, running, label: `${running}/${agents.length}` },
      tasks: { n: tasks.length, label: `${done}/${tasks.length}` },
      changes: { n: changes, label: `${changes}` },
      context: { n: pct === null ? 0 : 1, label: pct === null ? "-" : `${pct}%` },
      checks: { n: checks.length, label: `${passed}/${checks.length}` },
    };
  }

  // How full an agent's context window is: what its latest model call held over the window it plans for.
  // usedTokens is the running total of every call (a re-sent conversation counts again each step), so it
  // says what was spent, not what fits. null when the window is unknown.
  function ctxFill(a) {
    return a && a.window > 0 ? Math.min(100, ((a.contextTokens || 0) / a.window) * 100) : null;
  }

  function ctxLabel(a) {
    if (!a || (!a.window && !a.contextTokens)) return "";
    return fmtTokens(a.contextTokens || 0) + " / " + (a.window ? fmtTokens(a.window) : "?");
  }

  // Context usage percent of the fullest window among the agents; null when no window is known.
  function contextPct(s) {
    const fills = (s.agents || []).map(ctxFill).filter((f) => f !== null);
    return fills.length ? Math.round(Math.max.apply(null, fills)) : null;
  }

  // Context bar value: budget usage when known, else 0.
  function contextBarPct(s) {
    const p = contextPct(s);
    return p === null ? 0 : p;
  }

  function drawerRowsAgents(s) {
    const agents = s.agents || [];
    if (!agents.length) return `<div class="drawer-row mini">No agents</div>`;
    return agents.map((a) => {
      const model = a.escalatedFrom ? `${a.escalatedFrom} → ${a.model}` : (a.model || "");
      const name = esc(a.title || a.role || a.id);
      const stopable = a.status === "running" || a.status === "starting" || a.status === "queued";
      return `<div class="drawer-row" role="treeitem" aria-level="1">
        <span class="codicon ${statusIconCodicon(a.status)}" style="color:${statusColor(a.status)}" aria-hidden="true"></span>
        <span class="grow">${name}${a.role ? ` <span class="mini">${esc(titleCase(a.role))}</span>` : ""}</span>
        <span class="desc" title="${esc(model)}">${esc(model)}</span>
        <span class="actions">
          ${stopable ? rowBtn("command", "relo.agent.stop", a.id, "debug-stop", "Stop agent " + (a.title || a.id)) : ""}
          ${rowBtn("command", "relo.agent.refresh", a.id, "refresh", "Refresh context")}
          ${rowBtn("command", "relo.agent.inspect", a.id, "search", "Inspect context")}
          ${rowBtn("command", "relo.agent.openWorktree", a.id, "folder-opened", "Open worktree")}
        </span>
      </div>`;
    }).join("");
  }

  function drawerRowsTasks(s) {
    const tasks = s.tasks || [];
    if (!tasks.length) return `<div class="drawer-row mini">No tasks</div>`;
    return tasks.map((t) => {
      const deps = (t.dependencies || []).length;
      const diff = t.difficulty ? `<span class="badge" title="Difficulty">${esc(t.difficulty)}</span>` : "";
      return `<div class="drawer-row" role="treeitem" aria-level="1">
        ${mark(t.status)}
        <span class="grow" title="${esc(t.description || "")}">${esc(t.title || t.id)}</span>
        ${diff}
        ${deps ? `<span class="mini" title="Dependencies">${deps} dep${deps === 1 ? "" : "s"}</span>` : ""}
      </div>`;
    }).join("");
  }

  function drawerRowsContext(s) {
    const agents = (s.agents || []).filter((a) => a.window || a.contextTokens);
    const cache = (s.cache && s.cache.session) || null;
    const perAgent = ((s.cache && s.cache.agents) || []).reduce((m, x) => { m[x.agentId] = x; return m; }, {});
    const hit = cache ? Math.round((cache.hitRate || 0) * 100) : null;
    const rows = agents.map((a) => {
      const p = ctxFill(a) || 0;
      const ca = perAgent[a.id];
      const agentHit = ca ? ` · cache ${Math.round((ca.hitRate || 0) * 100)}%` : "";
      return `<div class="drawer-row" role="treeitem" aria-level="1" style="flex-wrap:wrap">
        <span class="grow">${esc(a.title || a.role || a.id)} <span class="mini">gen ${esc(a.generation || 0)}${esc(agentHit)}</span></span>
        <span class="mini" title="${attr(fmtTokens(a.usedTokens || 0) + " tokens used in total")}">${esc(ctxLabel(a))}</span>
        <span class="actions" style="display:inline-flex">${rowBtn("command", "relo.agent.refresh", a.id, "refresh", "Refresh context")}${rowBtn("command", "relo.agent.inspect", a.id, "search", "Inspect context")}</span>
        <div class="agent-track" style="width:100%"><div class="agent-bar" style="width:${p}%"></div></div>
      </div>`;
    }).join("");
    const head = hit === null ? "" : `<div class="drawer-row mini">Cache hit ${hit}%</div>`;
    return (head + rows) || `<div class="drawer-row mini">No context generation yet</div>`;
  }

  // A change waits for a decision until it is accepted or rejected. Direct-mode edits ("applied") are already on disk.
  function isDecided(c) {
    const st = String((c && c.status) || "").toLowerCase();
    return st === "accepted" || st === "rejected";
  }

  function drawerRowsChanges(s) {
    const changes = s.changes || [];
    if (!changes.length) return `<div class="drawer-row mini">No changes</div>`;
    const rows = changes.map((c) => {
      const status = String(c.status || "").toLowerCase();
      const applied = status === "applied";
      const decided = isDecided(c);
      const letter = letterFor(c.kind || c.status);
      const deco = applied
        ? `<span class="badge" title="applied">applied</span>`
        : `<span class="letter ${letter}" title="${esc(c.kind || c.status)}">${letter}</span>`;
      const off = applied ? ` disabled title="Applied directly; cannot revert" aria-label="Applied directly; cannot revert"` : decided ? ` disabled title="Already ${esc(status)}" aria-label="Already ${esc(status)}"` : "";
      const lines = (c.added || c.deleted) ? `<span class="mini">+${c.added || 0} -${c.deleted || 0}</span>` : "";
      const state = decided ? `<span class="mini">${esc(status)}</span>` : "";
      return `<div class="drawer-row${decided ? " dim" : ""}" role="treeitem" aria-level="1" data-key="ch-${attr(c.id)}">
        ${deco}
        <button type="button" class="link-btn grow" data-command="relo.changes.diff" data-id="${attr(c.id)}" title="Open diff">${esc(c.path)}</button>
        ${lines}${state}
        <span class="actions shown">
          ${rowBtn("command", "relo.changes.accept", c.id, "check", "Accept change", off)}
          ${rowBtn("command", "relo.changes.reject", c.id, "close", "Reject change", off)}
        </span>
      </div>`;
    }).join("");
    const pending = changes.filter((c) => !isDecided(c) && String(c.status || "").toLowerCase() !== "applied").length;
    const note = pending ? `<div class="drawer-row mini">Compared with your files when the agent started. Nothing here is in your files until you accept it.</div>` : "";
    const foot = pending > 1 ? `<div class="drawer-foot">
      <button type="button" class="secondary" data-cmd="relo.changes.acceptAll">Accept all (${pending})</button>
      <button type="button" class="secondary" data-cmd="relo.changes.rejectAll">Reject all</button>
    </div>` : "";
    return note + rows + foot;
  }

  function drawerRowsChecks(s) {
    const checks = s.verification || [];
    if (!checks.length) return `<div class="drawer-row mini">No checks run</div>`;
    return checks.map((v, i) => {
      const ok = v.status === "passed" || v.status === "pass";
      const icon = ok ? "codicon-check" : "codicon-error";
      const color = ok ? "var(--vscode-charts-green)" : "var(--vscode-charts-red)";
      const open = !!checkOpen[i];
      const out = v.output ? `<div class="check-out" id="chk-${i}"${open ? "" : " hidden"}>${esc(v.output)}</div>` : "";
      return `<div class="drawer-row" role="treeitem" aria-level="1">
        <button type="button" class="link-btn grow" data-toggle-check="${i}" aria-expanded="${open ? "true" : "false"}" aria-controls="chk-${i}"><span class="codicon ${icon}" style="color:${color}" aria-hidden="true"></span> ${esc(v.command || "check")}</button>
        <span class="mini">${esc(v.status || "")}</span>
      </div>${out}`;
    }).join("");
  }

  function letterFor(status) {
    const s = String(status || "").toLowerCase();
    if (s.startsWith("add") || s === "new" || s === "created") return "A";
    if (s.startsWith("del") || s === "removed") return "D";
    return "M";
  }

  function rowBtn(kind, command, id, icon, label, extra) {
    return `<button type="button" class="icon-btn" data-${kind}="${attr(command)}" data-id="${attr(id)}" aria-label="${attr(label)}" title="${attr(label)}"${extra || ""}><span class="codicon codicon-${icon}" aria-hidden="true"></span></button>`;
  }

  function drawerBody(name, s) {
    switch (name) {
      case "agents": return drawerRowsAgents(s);
      case "tasks": return drawerRowsTasks(s);
      case "context": return drawerRowsContext(s);
      case "changes": return drawerRowsChanges(s);
      case "checks": return drawerRowsChecks(s);
      default: return "";
    }
  }

  function drawerLabel(name) {
    return { agents: "Agents", tasks: "Tasks", context: "Context", changes: "Changes", checks: "Checks" }[name] || name;
  }

  // Compact status chips: label + count, open chip gets a bottom highlight (no chevrons, no pill).
  function strip(s) {
    const c = chipCounts(s);
    const chips = [
      ["agents", "Agents", c.agents.label, c.agents.n === 0],
      ["tasks", "Tasks", c.tasks.label, c.tasks.n === 0],
      ["changes", "Changes", c.changes.label, c.changes.n === 0],
      ["context", "Context", c.context.label, c.context.n === 0],
      ["checks", "Checks", c.checks.label, c.checks.n === 0],
    ];
    const chipsHtml = chips.map(([name, label, value, dim]) => {
      const open = openDrawerName === name;
      return `<button type="button" class="chip${dim ? " dim" : ""}${open ? " open" : ""}" data-drawer="${name}" aria-expanded="${open ? "true" : "false"}" aria-controls="drawer" aria-label="${esc(label)} ${esc(value)}" title="${esc(label)}: ${esc(value)}"><span>${esc(label)}</span> <span class="chip-value">${esc(value)}</span></button>`;
    }).join("");
    return `<div class="strip" role="toolbar" aria-label="Relo status">${chipsHtml}</div>
      <div class="ctx-bar" aria-hidden="true"><span style="width:${contextBarPct(s)}%"></span></div>
      <div class="drawer" id="drawer" role="region" aria-label="${openDrawerName ? esc(drawerLabel(openDrawerName)) : "Drawer"}"${openDrawerName ? "" : " hidden"}>${openDrawerName ? drawerBody(openDrawerName, s) : ""}</div>`;
  }

  function formatCache(s) {
    const c = (s && s.cache && s.cache.session) || {};
    const total = (c.input || 0) + (c.cacheRead || 0) + (c.cacheWrite || 0);
    const given = typeof c.hitRate === "number" ? c.hitRate : null;
    const pct = given !== null ? given * 100 : (total ? (c.cacheRead || 0) * 100 / total : 0);
    return [`Cache ${pct.toFixed(0)}%`, `In ${fmtTokens(c.input)}`, `Out ${fmtTokens(c.output)}`];
  }

  function cacheTitle() {
    return "Cache hit: cacheRead / (input + cacheRead + cacheWrite)";
  }

  function currentMode() {
    return (state.session && state.session.mode) || "build";
  }

  // The only mode control: chat and dashboard composers both use this.
  function modeSelect(mode) {
    return `<select id="mode" class="mode-select" aria-label="Mode">
            <option value="build"${sel(mode, "build")}>Build</option>
            <option value="plan"${sel(mode, "plan")}>Plan</option>
            <option value="review"${sel(mode, "review")}>Review</option>
          </select>`;
  }

  function sessionTitle(s) {
    const obj = s && s.session && s.session.objective;
    return obj ? String(obj).split("\n")[0].slice(0, 80) : "Relo Code";
  }

  function toolbar(s) {
    const menuItems = [
      ["pause", "Pause", "debug-pause"],
      ["resume", "Resume", "debug-continue"],
      ["stopAll", "Stop all agents", "debug-stop"],
      ["refreshPrimary", "Refresh primary context", "refresh"],
      ["restartBackend", "Restart backend", "debug-restart"],
      ["openLogs", "Show logs", "output"],
      ["showCacheStats", "Cache stats", "graph"],
    ];
    const items = menuItems.map(([cmdId, label, icon], i) => `<button type="button" role="menuitem" tabindex="-1" data-menu-cmd="relo.${cmdId}" data-index="${i}"><span class="codicon codicon-${icon}" aria-hidden="true"></span>${esc(label)}</button>`).join("");
    const settingsActive = page === "settings";
    const historyActive = page === "history";
    const title = settingsActive ? "Settings" : historyActive ? "Session history" : sessionTitle(s);
    return `<div class="header" title="${esc(title)}">
      <span class="status-dot ${statusCls()}" aria-hidden="true"></span>
      <span class="header-title header-session">${esc(title)}</span>
      <span class="header-spacer"></span>
      <button type="button" class="icon-btn" data-cmd="relo.newSession" aria-label="New session" title="New session"><span class="codicon codicon-add" aria-hidden="true"></span></button>
      <button type="button" class="icon-btn" data-page="${historyActive ? "chat" : "history"}" aria-label="${historyActive ? "Back to chat" : "Session history"}" title="${historyActive ? "Back to chat" : "Session history"}"${historyActive ? ` aria-pressed="true"` : ""}><span class="codicon ${historyActive ? "codicon-arrow-left" : "codicon-history"}" aria-hidden="true"></span></button>
      <button type="button" class="icon-btn" data-cmd="relo.dashboard" aria-label="Open dashboard" title="Open dashboard"><span class="codicon codicon-layout" aria-hidden="true"></span></button>
      <button type="button" class="icon-btn" data-page="${settingsActive ? "chat" : "settings"}" aria-label="${settingsActive ? "Back to chat" : "Settings"}" title="${settingsActive ? "Back to chat" : "Settings"}"${settingsActive ? ` aria-pressed="true"` : ""}><span class="codicon ${settingsActive ? "codicon-arrow-left" : "codicon-gear"}" aria-hidden="true"></span></button>
      <button type="button" class="icon-btn" id="more" aria-haspopup="menu" aria-expanded="${menuOpen ? "true" : "false"}" aria-controls="more-menu" aria-label="More actions" title="More actions"><span class="codicon codicon-ellipsis" aria-hidden="true"></span></button>
      <div class="menu" id="more-menu" role="menu" aria-label="More actions"${menuOpen ? "" : " hidden"}>${items}</div>
    </div>`;
  }

  function chat() {
    const mode = currentMode();
    const msgs = state.messages || [];
    const emptyEl = msgs.length === 0 ? empty("Describe a task to start.") : "";
    const running = isRunning(state);
    const sendIcon = running ? "codicon-debug-stop" : "codicon-send";
    const sendLabel = running ? "Stop" : "Send message";
    const eff = (state.efficiency && state.efficiency.preset) || "off";
    return `${toolbar(state)}
    ${strip(state)}
    <div class="messages" role="log" aria-label="Chat messages">${emptyEl}${msgs.map(msgHtml).join("")}</div>
    ${runPanelHtml(state, Date.now(), runUi)}
    <div class="composer">
      <textarea id="box" rows="1" placeholder="Ask Relo…" aria-label="Message"></textarea>
      <div class="toolbar">
        <div class="toolbar-left">
          ${modeSelect(mode)}
          <button type="button" class="link-btn model-label" id="model" title="Select primary model" aria-label="Primary model: ${attr(primaryModelName())}. Select">${esc(modelTail())}<span class="codicon codicon-chevron-down" aria-hidden="true"></span></button>
        </div>
        <div class="toolbar-right">
          <button type="button" class="eff-link" id="eff" title="Token saving settings" aria-label="Token saving: ${attr(titleCase(eff))}. Open settings">Token saving: ${esc(titleCase(eff))}</button>
          <button type="button" id="send" class="send" aria-label="${sendLabel}" title="${sendLabel}"><span class="codicon ${sendIcon}"></span><span class="sr-only">${sendLabel}</span></button>
        </div>
      </div>
    </div>
    <div class="cache-footer" title="${esc(cacheTitle())}">${formatCache(state).map((t) => `<span>${esc(t)}</span>`).join("")}</div>`;
  }

  // Session history -----------------------------------------------------------

  // Rows come from the extension (core session.list); the list is fetched whenever the page opens.
  const historyUi = { sessions: null, error: "" };

  // "5 min ago" style age for a timestamp; falls back to the date after a week.
  function relTime(iso, nowMs) {
    const t = typeof iso === "number" ? iso : Date.parse(iso);
    if (!t) return "";
    const sec = Math.max(0, Math.round(((nowMs == null ? Date.now() : nowMs) - t) / 1000));
    if (sec < 45) return "just now";
    if (sec < 3600) return Math.max(1, Math.round(sec / 60)) + " min ago";
    if (sec < 86400) return Math.round(sec / 3600) + " h ago";
    if (sec < 7 * 86400) return Math.round(sec / 86400) + " d ago";
    return new Date(t).toLocaleDateString();
  }

  function historyRow(h, now) {
    const pending = h.pendingChanges > 0 ? `<span class="badge" title="Changes waiting for a decision">${h.pendingChanges}</span>` : "";
    const meta = [relTime(h.updatedAt, now), plural(h.messages || 0, "message")].filter(Boolean).join(" · ") + (h.current ? " · open now" : "");
    return `<li class="hist-row${h.current ? " current" : ""}" data-key="hs-${attr(h.id)}">
      <button type="button" class="hist-open" data-session-open="${attr(h.id)}" data-current="${h.current ? "1" : "0"}"${h.current ? ` aria-current="true"` : ""}>
        <span class="hist-name">${esc(h.title || "Untitled session")}</span>
        <span class="hist-meta">${esc(meta)}</span>
      </button>${pending}
      <button type="button" class="icon-btn" data-session-delete="${attr(h.id)}" aria-label="Delete session" title="Delete session"><span class="codicon codicon-trash" aria-hidden="true"></span></button>
    </li>`;
  }

  function historyPage(ui, now) {
    const h = ui || historyUi;
    let body;
    if (h.error) body = `<div class="hist-empty" role="alert">${esc(h.error)}</div>`;
    else if (!h.sessions) body = `<div class="hist-empty">Loading…</div>`;
    else if (!h.sessions.length) body = `<div class="hist-empty">No earlier sessions yet. Every session you start is kept here so you can come back to it.</div>`;
    else body = `<ul class="hist-list" role="list">${h.sessions.map((x) => historyRow(x, now)).join("")}</ul>`;
    return `<div class="hist" id="history-page">
      <div class="hist-head"><span class="hist-title">Sessions in this workspace</span><button type="button" class="secondary" data-cmd="relo.newSession">New session</button></div>
      ${body}
    </div>`;
  }

  // Settings page -------------------------------------------------------------

  // Groups in display order. Each setting key is listed explicitly so unknown keys never render.
  const SETTINGS_GROUPS = [
    { id: "models", title: "Models", keys: ["relo.routing.mode", "relo.primaryModel", "relo.tier1Model", "relo.tier2Model", "relo.routing.byDifficulty", "relo.routing.escalate", "relo.models.contextLimit"] },
    { id: "providers", title: "Providers", keys: [] },
    { id: "efficiency", title: "Token saving", keys: ["relo.efficiency.preset", "relo.efficiency.voice", "relo.efficiency.craft", "relo.efficiency.planLean", "relo.efficiency.roles"] },
    { id: "agents", title: "Agents", keys: ["relo.maxConcurrentAgents", "relo.maxTier1Agents", "relo.maxTier2Agents", "relo.maxHierarchyDepth", "relo.maxRetries"] },
    { id: "context", title: "Context", keys: ["relo.context.autoRefresh", "relo.context.refreshThreshold", "relo.context.maxUtilization", "relo.context.compressionStrategy", "relo.context.keepRawHistory", "relo.context.projectMemory"] },
    { id: "verification", title: "Verification", keys: ["relo.autoVerify", "relo.autoReview", "relo.report.maxChars"] },
    { id: "permissions", title: "Permissions", keys: ["relo.permissions.read", "relo.permissions.write", "relo.permissions.test", "relo.permissions.build", "relo.permissions.install", "relo.permissions.delete", "relo.permissions.gitCommit", "relo.permissions.gitPush", "relo.permissions.network", "relo.permissions.outside", "relo.permissions.destructive"] },
    { id: "advanced", title: "Advanced", keys: ["relo.runtime", "relo.openCode.path", "relo.logLevel"] },
  ];

  // Short labels for keys whose property name is not readable on its own.
  const SETTING_LABELS = {
    "relo.routing.byDifficulty": "Routing by difficulty",
    "relo.routing.escalate": "Escalate on failure",
    "relo.routing.mode": "Routing mode",
    "relo.models.contextLimit": "Context limit",
    "relo.efficiency.preset": "Saving level",
    "relo.efficiency.voice": "Reply brevity",
    "relo.efficiency.craft": "Minimal code",
    "relo.efficiency.planLean": "Fewer agents",
    "relo.efficiency.roles": "Per-role overrides",
    "relo.context.autoRefresh": "Auto refresh",
    "relo.context.refreshThreshold": "Refresh threshold",
    "relo.context.maxUtilization": "Max utilization",
    "relo.context.compressionStrategy": "Compression strategy",
    "relo.context.keepRawHistory": "Keep raw history",
    "relo.context.projectMemory": "Project memory",
    "relo.report.maxChars": "Report max chars",
    "relo.openCode.path": "OpenCode path",
    "relo.logLevel": "Log level",
    "relo.primaryModel": "Primary model",
    "relo.tier1Model": "Tier 1 model (medium and hard tasks)",
    "relo.tier2Model": "Tier 2 model (trivial and easy tasks, probes)",
  };

  // Rows whose control gets the full width under the label.
  const WIDE_KEYS = ["relo.routing.mode", "relo.primaryModel", "relo.tier1Model", "relo.tier2Model", "relo.routing.byDifficulty"];
  const SIMPLE_ONLY = ["relo.tier1Model", "relo.tier2Model"];
  const ADVANCED_ONLY = ["relo.routing.byDifficulty"];

  const DIFFICULTY_LEVELS = ["trivial", "easy", "medium", "hard", "expert"];

  function titleCase(s) {
    return String(s ?? "").replace(/\b\w/g, (c) => c.toUpperCase());
  }

  function settingLabel(key) {
    if (SETTING_LABELS[key]) return SETTING_LABELS[key];
    const sub = key.slice("relo.".length).split(".").pop();
    const spaced = sub.replace(/([A-Z])/g, " $1").toLowerCase();
    return spaced.charAt(0).toUpperCase() + spaced.slice(1);
  }

  function schemaOf(key) {
    return (settingsCache && settingsCache.schema && settingsCache.schema[key]) || {};
  }

  function valueOf(key) {
    return settingsCache && settingsCache.values ? settingsCache.values[key] : undefined;
  }

  function defaultOf(key) {
    return settingsCache && settingsCache.defaults ? settingsCache.defaults[key] : undefined;
  }

  function isModified(key) {
    const v = valueOf(key);
    const d = defaultOf(key);
    if (v === undefined) return false;
    return JSON.stringify(v) !== JSON.stringify(d);
  }


  // Model metadata --------------------------------------------------------------
  const RESERVED_PROVIDERS = ["anthropic", "openai", "google", "openrouter", "opencode", "azure", "amazon-bedrock", "groq", "mistral", "xai", "deepseek", "github-copilot"];
  const EFFORT_SHORT = { medium: "med", minimal: "min" };

  function modelInfo(ref) {
    const models = (settingsCache && settingsCache.models) || [];
    return models.find((m) => `${m.provider}/${m.id}` === ref) || null;
  }

  function effortList(efforts) {
    return (efforts || []).map((e) => EFFORT_SHORT[e] || e).join("/");
  }

  // "ctx 200k · low/med/high", or "" when nothing is known.
  function modelHint(m) {
    if (!m) return "";
    const parts = [];
    if (m.context > 0) parts.push("ctx " + fmtTokens(m.context));
    if (m.efforts && m.efforts.length) parts.push(effortList(m.efforts));
    return parts.join(" · ");
  }

  function contextLimit() {
    const v = Number(valueOf("relo.models.contextLimit"));
    return v > 0 ? v : 0;
  }

  // Same rule as the core: smaller of global limit and model max; 0 means unknown.
  function effectiveWindow(max) {
    const cap = contextLimit();
    if (cap > 0 && max > 0) return Math.min(cap, max);
    return cap > 0 ? cap : max > 0 ? max : 0;
  }

  function routingMode() {
    return valueOf("relo.routing.mode") === "advanced" ? "advanced" : "simple";
  }

  // Collapse state -------------------------------------------------------------

  const COLLAPSE_KEY_RE = /^[gp]:[A-Za-z0-9._@\/-]{1,80}$/;

  function cleanCollapsed(raw) {
    const out = {};
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) return out;
    Object.keys(raw).slice(0, 200).forEach((k) => { if (COLLAPSE_KEY_RE.test(k) && raw[k] === true) out[k] = true; });
    return out;
  }

  function isCollapsed(key) {
    return uiCollapsed[key] === true;
  }

  function setCollapsed(key, on) {
    if (on) uiCollapsed[key] = true; else delete uiCollapsed[key];
    vscode.postMessage({ type: "ui.collapse", key, collapsed: !!on });
  }

  // Model picker ---------------------------------------------------------------

  // Every whitespace-separated word must appear in "provider/id name", in any order, any case.
  function filterModels(models, query) {
    const words = String(query || "").toLowerCase().split(/\s+/).filter(Boolean);
    return models.filter((m) => {
      const hay = `${m.provider}/${m.id} ${m.name || ""}`.toLowerCase();
      return words.every((w) => hay.indexOf(w) >= 0);
    });
  }

  // Flat row model for the picker: optional blank row, then provider headers each followed by their models.
  // While searching every matching provider is expanded; otherwise the saved collapse state applies.
  function pickerRows(models, query, current, blank, collapsed) {
    const searching = String(query || "").trim() !== "";
    const all = models.slice();
    if (current && !all.some((m) => `${m.provider}/${m.id}` === current)) {
      const [prov, id] = current.indexOf("/") > 0 ? [current.slice(0, current.indexOf("/")), current.slice(current.indexOf("/") + 1)] : ["other", current];
      all.push({ provider: prov, id, name: "" });
    }
    const rows = [];
    if (!searching) rows.push({ kind: "opt", ref: "", label: blank, key: "o:" });
    const byProv = {};
    all.forEach((m) => { (byProv[m.provider] = byProv[m.provider] || []).push(m); });
    let matches = 0;
    Object.keys(byProv).sort((x, y) => x.localeCompare(y)).forEach((prov) => {
      const found = searching ? filterModels(byProv[prov], query) : byProv[prov];
      if (!found.length) return;
      matches += found.length;
      const open = searching || !(collapsed && collapsed["p:" + prov]);
      rows.push({ kind: "group", provider: prov, count: found.length, open, toggle: !searching, key: "g:" + prov });
      if (open) found.forEach((m) => rows.push({ kind: "opt", ref: `${prov}/${m.id}`, label: m.id, m, key: "o:" + prov + "/" + m.id }));
    });
    return { rows, matches, searching };
  }

  // Searching: first matching model, so Enter picks it. Browsing: the current model, else the first row.
  function defaultActive(res, current) {
    const opt = (r) => r.kind === "opt";
    const pick = res.searching
      ? res.rows.find((r) => opt(r) && r.ref)
      : res.rows.find((r) => opt(r) && r.ref && r.ref === current) || res.rows[0];
    return pick ? pick.key : "";
  }

  // Rows the arrow keys can land on: while searching, the static provider headers are skipped.
  function navKeys(res) {
    return res.rows.filter((r) => !(r.kind === "group" && !r.toggle)).map((r) => r.key);
  }

  function pickerListHtml(pid, rows, current, activeKey) {
    return rows.map((r, i) => {
      const id = `${pid}-r${i}`;
      const act = r.key === activeKey ? " active" : "";
      if (r.kind === "group") {
        return `<li role="treeitem" id="${id}" aria-level="1" aria-expanded="${r.open}" class="mp-group${act}${r.toggle ? "" : " static"}" data-rowkey="${attr(r.key)}"><span class="codicon ${r.open ? "codicon-chevron-down" : "codicon-chevron-right"}" aria-hidden="true"></span><span class="mp-prov">${esc(r.provider)}</span><span class="badge">${r.count}</span></li>`;
      }
      const sel = r.ref === current;
      const hint = r.m ? modelHint(r.m) : "";
      return `<li role="treeitem" id="${id}" aria-level="${r.ref ? 2 : 1}" aria-selected="${sel}" class="mp-opt${r.ref ? "" : " mp-blank"}${act}${sel ? " selected" : ""}" data-rowkey="${attr(r.key)}" data-ref="${attr(r.ref)}"><span class="mp-id">${esc(r.label)}</span>${hint ? `<span class="mini mp-hint">${esc(hint)}</span>` : ""}${sel ? `<span class="codicon codicon-check mp-check" aria-hidden="true"></span>` : ""}</li>`;
    }).join("");
  }

  function pickerCountText(res, total) {
    if (!res.matches) return res.searching ? "No models match" : "No models";
    return res.searching ? `${res.matches} of ${total} models` : "";
  }

  function pickerClosedHtml(pid, dataAttrs, label, shown) {
    return `<div class="mpick" ${dataAttrs}><button type="button" class="mpick-btn" id="${pid}-btn" role="combobox" aria-haspopup="tree" aria-expanded="false" aria-label="${attr(label)}: ${attr(shown)}" title="${attr(shown)}"><span class="mpick-val">${esc(shown)}</span><span class="codicon codicon-chevron-down" aria-hidden="true"></span></button></div>`;
  }

  function pickerOpenHtml(pid, dataAttrs, label, models, current, blank, picker, collapsed) {
    const res = pickerRows(models, picker.query, current, blank, collapsed);
    const keys = res.rows.map((r) => r.key);
    const activeKey = keys.indexOf(picker.active) >= 0 ? picker.active : defaultActive(res, current);
    const ai = keys.indexOf(activeKey);
    return `<div class="mpick open" ${dataAttrs}><input type="text" class="mpick-q" id="${pid}-q" role="combobox" aria-haspopup="tree" aria-expanded="true" aria-controls="${pid}-list"${ai >= 0 ? ` aria-activedescendant="${pid}-r${ai}"` : ""} aria-label="Search ${attr(label)}" placeholder="Search models" autocomplete="off" spellcheck="false" value="${attr(picker.query)}"/><ul class="mpick-list" id="${pid}-list" role="tree" aria-label="${attr(label)}">${pickerListHtml(pid, res.rows, current, activeKey)}</ul><div class="mini mpick-count" id="${pid}-count" role="status">${esc(pickerCountText(res, models.length))}</div></div>`;
  }

  // One picker per model setting; level is set for the per-difficulty rows.
  function modelPickerHtml(key, level, current, blank) {
    const pid = "mp-" + (key + (level ? "-" + level : "")).replace(/[^a-zA-Z0-9]/g, "-");
    const dataAttrs = `data-pid="${pid}" data-mkey="${attr(key)}"${level ? ` data-mlevel="${attr(level)}"` : ""} data-blank="${attr(blank)}"`;
    const label = level ? `Model for ${level} tasks` : settingLabel(key);
    if (!openPicker || openPicker.id !== pid) return pickerClosedHtml(pid, dataAttrs, label, current || blank);
    return pickerOpenHtml(pid, dataAttrs, label, (settingsCache && settingsCache.models) || [], current, blank, openPicker, uiCollapsed);
  }

  // Secondary line under a model picker: effective window and an effort picker when the model has levels.
  function modelDetailHtml(ref) {
    const m = modelInfo(ref);
    if (!ref) return "";
    const bits = [];
    if (m && m.context > 0) {
      const eff = effectiveWindow(m.context);
      bits.push(eff < m.context ? `window ${fmtTokens(eff)} (model max ${fmtTokens(m.context)})` : `window ${fmtTokens(m.context)}`);
    } else if (contextLimit() > 0) {
      bits.push(`window ${fmtTokens(contextLimit())} (model max unknown)`);
    } else {
      bits.push("window unknown");
    }
    let effort = "";
    if (m && m.efforts && m.efforts.length) {
      const map = valueOf("relo.models.efforts") && typeof valueOf("relo.models.efforts") === "object" ? valueOf("relo.models.efforts") : {};
      const cur = typeof map[ref] === "string" ? map[ref] : "";
      effort = `<label class="effort-line"><span class="mini">Effort</span><select data-key="relo.models.efforts" data-level="${attr(ref)}" aria-label="Reasoning effort for ${attr(ref)}"><option value="">(model default)</option>${m.efforts.map((e) => `<option value="${attr(e)}"${e === cur ? " selected" : ""}>${esc(e)}</option>`).join("")}</select></label>`;
    }
    return `<div class="mini model-detail">${esc(bits.join(" · "))}</div>${effort}`;
  }

  function modeToggleHtml(value) {
    const cur = value === "advanced" ? "advanced" : "simple";
    const btn = (id, label) => `<button type="button" role="radio" aria-checked="${cur === id}" data-mode="${id}" tabindex="${cur === id ? 0 : -1}">${label}</button>`;
    return `<div class="seg" role="radiogroup" aria-label="Routing mode">${btn("simple", "Simple")}${btn("advanced", "Advanced")}</div>`;
  }

  // Providers -------------------------------------------------------------------

  function slugify(name) {
    const s = String(name || "").toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^[^a-z0-9]+/, "").replace(/-+$/, "");
    return s.slice(0, 40);
  }

  function providerList() {
    const v = valueOf("relo.providers");
    return Array.isArray(v) ? v : [];
  }

  function hasKey(id) {
    return !!(settingsCache && settingsCache.providerKeys && settingsCache.providerKeys[id]);
  }

  function newProviderForm() {
    return { editing: "", name: "", id: "", idTouched: false, kind: "openai", baseURL: "", models: [], busy: false, error: "", note: "", requestId: "" };
  }

  function formFromProvider(p) {
    return {
      editing: p.id, name: p.name || p.id, id: p.id, idTouched: true, kind: p.kind === "anthropic" ? "anthropic" : "openai", baseURL: p.baseURL || "",
      models: (p.models || []).map((m) => ({ id: m.id, name: m.name || "", context: m.context ? String(m.context) : "", output: m.output ? String(m.output) : "", efforts: (m.efforts || []).join(", ") })),
      busy: false, error: "", note: "", requestId: "",
    };
  }

  function parseCount(v) {
    const n = Math.floor(Number(String(v || "").replace(/[,_\s]/g, "")));
    return n > 0 && n <= 10000000 ? n : 0;
  }

  function parseEfforts(v) {
    const out = [];
    String(v || "").toLowerCase().split(/[,\s]+/).forEach((e) => { if (/^[a-z0-9][a-z0-9_-]{0,19}$/.test(e) && out.indexOf(e) < 0 && out.length < 10) out.push(e); });
    return out;
  }

  function providerFromForm(f) {
    return {
      id: f.id, name: f.name.trim() || f.id, kind: f.kind, baseURL: f.baseURL.trim(),
      models: f.models.filter((m) => m.id.trim()).map((m) => {
        const efforts = parseEfforts(m.efforts);
        const out = { id: m.id.trim(), name: (m.name || "").trim() || m.id.trim() };
        const c = parseCount(m.context);
        const o = parseCount(m.output);
        if (c) out.context = c;
        if (o) out.output = o;
        if (efforts.length) { out.reasoning = true; out.efforts = efforts; }
        return out;
      }),
    };
  }

  function providerFormIssue(f) {
    if (!/^[a-z0-9][a-z0-9_-]{0,39}$/.test(f.id)) return "Id: lowercase letters, digits, - or _ (max 40).";
    if (!f.editing && RESERVED_PROVIDERS.indexOf(f.id) >= 0) return "Id is reserved for a built-in provider.";
    if (!f.editing && providerList().some((p) => p.id === f.id)) return "A provider with this id already exists.";
    try {
      const u = new URL(f.baseURL.trim());
      if (u.protocol !== "http:" && u.protocol !== "https:") return "Base URL must be http or https.";
      if (u.username || u.password) return "Base URL must not contain credentials.";
    } catch (_e) {
      return "Base URL is not a valid URL.";
    }
    const ids = {};
    for (const m of f.models) {
      const id = m.id.trim();
      if (!id) continue;
      if (/[\s"'`\\]/.test(id)) return `Model id "${id}" has spaces or quotes.`;
      if (ids[id]) return `Duplicate model id "${id}".`;
      ids[id] = true;
    }
    return "";
  }

  const PROVIDER_ROWS_SHOWN = 12;

  function providerRowsHtml() {
    return providerList().map((p) => {
      const key = hasKey(p.id);
      const n = (p.models || []).length;
      const confirm = providerRemoveConfirm === p.id;
      const actions = confirm
        ? `<span class="mini">Remove?</span><button type="button" class="secondary" data-prov-remove-yes="${attr(p.id)}">Remove</button><button type="button" class="secondary" data-prov-remove-no="1">Keep</button>`
        : `<button type="button" class="icon-btn" data-prov-edit="${attr(p.id)}" aria-label="Edit ${attr(p.name || p.id)}" title="Edit"><span class="codicon codicon-edit" aria-hidden="true"></span></button><button type="button" class="icon-btn" data-prov-remove="${attr(p.id)}" aria-label="Remove ${attr(p.name || p.id)}" title="Remove"><span class="codicon codicon-trash" aria-hidden="true"></span></button>`;
      return `<div class="setting prov" data-search="${attr(`${p.name} ${p.id} ${p.baseURL} ${p.kind} provider`.toLowerCase())}">
        <div class="setting-label prov-head"><span class="prov-name">${esc(p.name || p.id)}</span><span class="badge">${p.kind === "anthropic" ? "Anthropic" : "OpenAI"}</span></div>
        <div class="setting-desc prov-url" title="${attr(p.baseURL)}">${esc(p.baseURL)}</div>
        <div class="mini prov-meta"><span class="${key ? "key-ok" : "key-missing"}">${key ? "Key set" : "No key"}</span> · ${plural(n, "model")}</div>
        <div class="prov-actions">${actions}</div>
      </div>`;
    }).join("");
  }

  function modelRowsHtml(f) {
    const shown = providerShowAll ? f.models.length : Math.min(f.models.length, PROVIDER_ROWS_SHOWN);
    const rows = f.models.slice(0, shown).map((m, i) => `<div class="pm-row" data-pm="${i}">
        <div class="pm-top"><input type="text" id="pm-id-${i}" data-pm-field="id" data-pm-index="${i}" value="${attr(m.id)}" placeholder="model id" aria-label="Model id"/><button type="button" class="icon-btn" data-pm-remove="${i}" aria-label="Remove model ${attr(m.id)}" title="Remove model"><span class="codicon codicon-close" aria-hidden="true"></span></button></div>
        <div class="pm-grid">
          <input type="text" id="pm-ctx-${i}" data-pm-field="context" data-pm-index="${i}" value="${attr(m.context)}" placeholder="context" inputmode="numeric" aria-label="Context window tokens for ${attr(m.id)}"/>
          <input type="text" id="pm-out-${i}" data-pm-field="output" data-pm-index="${i}" value="${attr(m.output)}" placeholder="output" inputmode="numeric" aria-label="Max output tokens for ${attr(m.id)}"/>
          <input type="text" id="pm-eff-${i}" data-pm-field="efforts" data-pm-index="${i}" value="${attr(m.efforts)}" placeholder="efforts: low, high" aria-label="Reasoning efforts for ${attr(m.id)}"/>
        </div>
      </div>`).join("");
    const more = f.models.length > shown ? `<button type="button" class="text-link" data-pm-all="1">Show all ${f.models.length} models</button>` : "";
    return rows + more;
  }

  function providerFormHtml(f) {
    const issue = providerFormIssue(f);
    const keyPlaceholder = f.editing && hasKey(f.editing) ? "Key set. Leave blank to keep it." : "API key";
    return `<div class="setting wide pform" id="provider-form" data-search="add provider custom endpoint">
      <div class="setting-label">${f.editing ? "Edit provider" : "Add provider"}</div>
      <div class="pf-fields">
        <label class="pf-field"><span class="mini">Name</span><input type="text" id="pf-name" data-pf="name" value="${attr(f.name)}" autocomplete="off"/></label>
        <label class="pf-field"><span class="mini">Id</span><input type="text" id="pf-id" data-pf="id" value="${attr(f.id)}" autocomplete="off"${f.editing ? " disabled" : ""}/></label>
        <label class="pf-field"><span class="mini">Kind</span><select id="pf-kind" data-pf="kind"><option value="openai"${f.kind === "openai" ? " selected" : ""}>OpenAI-compatible</option><option value="anthropic"${f.kind === "anthropic" ? " selected" : ""}>Anthropic-compatible</option></select></label>
        <label class="pf-field"><span class="mini">Base URL</span><input type="text" id="pf-url" data-pf="baseURL" value="${attr(f.baseURL)}" placeholder="https://host/v1" autocomplete="off" spellcheck="false"/></label>
        <label class="pf-field"><span class="mini">API key</span><input type="password" id="pf-key" data-pf-key="1" placeholder="${attr(keyPlaceholder)}" autocomplete="off" spellcheck="false"/></label>
        <div class="pf-actions"><button type="button" class="secondary" id="pf-fetch"${f.busy ? " disabled" : ""}>${f.busy ? "Fetching…" : "Fetch models"}</button><span class="mini">${f.models.length ? plural(f.models.length, "model") : "No models yet"}</span></div>
        ${f.error ? `<div class="pf-error" role="alert">${esc(f.error)}</div>` : ""}${f.note ? `<div class="mini" role="status">${esc(f.note)}</div>` : ""}
        <div class="pf-models">${modelRowsHtml(f)}</div>
        <button type="button" class="text-link" id="pf-addmodel">Add model by id</button>
        ${issue && (f.id || f.baseURL) ? `<div class="mini pf-issue">${esc(issue)}</div>` : ""}
        <div class="pf-actions"><button type="button" id="pf-save"${issue ? " disabled" : ""}>Save</button><button type="button" class="secondary" id="pf-cancel">Cancel</button></div>
      </div>
    </div>`;
  }

  function providersBodyHtml() {
    const rows = providerRowsHtml();
    const empty = providerList().length === 0 && !providerForm ? `<div class="mini prov-empty">No custom providers. Add an OpenAI- or Anthropic-compatible endpoint.</div>` : "";
    const add = providerForm ? providerFormHtml(providerForm) : `<div class="setting wide" data-search="add provider custom endpoint api key"><button type="button" class="secondary" id="prov-add"><span class="codicon codicon-add" aria-hidden="true"></span> Add provider</button></div>`;
    return rows + empty + add;
  }

  // One control per setting. Dropdowns come from schema enums or the model list.
  function controlHtml(key, value) {
    const sc = schemaOf(key);
    const id = "s-" + key.replace(/[^a-zA-Z0-9]/g, "-");
    const label = `<span class="sr-only">${esc(settingLabel(key))}</span>`;
    if (key === "relo.routing.mode") return modeToggleHtml(value);
    if (key === "relo.primaryModel" || key === "relo.tier1Model" || key === "relo.tier2Model") {
      const current = typeof value === "string" ? value : "";
      return modelPickerHtml(key, "", current, "(none)") + modelDetailHtml(current);
    }
    if (key === "relo.routing.byDifficulty") {
      const map = value && typeof value === "object" && !Array.isArray(value) ? value : {};
      return DIFFICULTY_LEVELS.map((lvl) => {
        const cur = typeof map[lvl] === "string" ? map[lvl] : "";
        return `<div class="table-line"><span class="mini lvl">${esc(lvl)}</span>${modelPickerHtml(key, lvl, cur, "(default)")}</div>`;
      }).join("");
    }
    if (key === "relo.efficiency.roles") {
      return `<span class="mini">per-role table, edit in VS Code settings</span>`;
    }
    if (sc.type === "boolean") {
      return `<input id="${id}" type="checkbox" data-key="${esc(key)}"${value ? " checked" : ""} aria-label="${esc(settingLabel(key))}"/>`;
    }
    if (sc.enum) {
      return `<select id="${id}" data-key="${esc(key)}" aria-label="${esc(settingLabel(key))}">${sc.enum.map((o) => `<option value="${attr(o)}"${o === value ? " selected" : ""}>${esc(titleCase(o))}</option>`).join("")}</select>`;
    }
    if (sc.type === "number") {
      const min = sc.minimum !== undefined ? ` min="${attr(sc.minimum)}"` : "";
      const max = sc.maximum !== undefined ? ` max="${attr(sc.maximum)}"` : "";
      return `<input id="${id}" type="number" step="any" data-key="${esc(key)}" data-type="number" value="${attr(value ?? "")}"${min}${max} aria-label="${esc(settingLabel(key))}"/>`;
    }
    if (key === "relo.routing.escalate") {
      return `<input id="${id}" type="checkbox" data-key="${esc(key)}"${value ? " checked" : ""} aria-label="${esc(settingLabel(key))}"/>`;
    }
    return `<input id="${id}" type="text" data-key="${esc(key)}" data-type="text" value="${attr(value ?? "")}" aria-label="${esc(settingLabel(key))}"/>`;
  }

  function settingRow(key) {
    const sc = schemaOf(key);
    const desc = sc.description || "";
    const modified = isModified(key);
    const label = settingLabel(key);
    const searchText = `${label} ${key} ${desc}`.toLowerCase();
    const wide = WIDE_KEYS.indexOf(key) >= 0;
    return `<div class="setting${wide ? " wide" : ""}${modified ? " modified" : ""}" data-search="${attr(searchText)}">
      <div class="setting-label">${esc(label)}</div>
      <div class="setting-desc">${esc(desc)}</div>
      <div class="setting-ctl">${controlHtml(key, valueOf(key))}
        <button type="button" class="icon-btn setting-reset" data-reset="${attr(key)}" aria-label="Reset ${attr(label)} to default" title="Reset to default"${modified ? "" : " hidden"}><span class="codicon codicon-discard" aria-hidden="true"></span></button>
      </div>
    </div>`;
  }

  function settingsPage() {
    if (!settingsCache) return `<div class="settings-empty">Loading settings…</div>`;
    const groups = SETTINGS_GROUPS.map((g) => {
      const collapsed = isCollapsed("g:" + g.id);
      const adv = routingMode() === "advanced";
      const keys = g.keys.filter((k) => !(adv ? SIMPLE_ONLY : ADVANCED_ONLY).includes(k));
      const rows = (g.id === "providers" ? providersBodyHtml() : keys.filter((k) => settingsCache.schema && settingsCache.schema[k] || k === "relo.routing.byDifficulty" || k === "relo.efficiency.roles").map(settingRow).join(""));
      return `<section class="settings-group" data-group="${g.id}" aria-label="${esc(g.title)}">
        <h3 role="button" tabindex="0" aria-expanded="${collapsed ? "false" : "true"}" data-collapse="${g.id}"><span class="codicon ${collapsed ? "codicon-chevron-right" : "codicon-chevron-down"}" aria-hidden="true"></span>${esc(g.title)}</h3>
        <div class="settings-body"${collapsed ? " hidden" : ""}>${rows}</div>
      </section>`;
    }).join("");
    return `<div class="settings" id="settings-page">
      <div class="settings-head">
        <input type="search" id="settings-search" placeholder="Search settings" aria-label="Search settings" value="${attr(settingsQuery)}"/>
      </div>
      ${groups}
      <div class="settings-empty" id="settings-none" hidden>No matching settings</div>
      <div class="settings-foot">
        <button type="button" class="secondary" data-cmd="relo.restartBackend">Restart backend</button>
        <button type="button" class="text-link" data-settings-open="1">Open all settings in VS Code</button>
      </div>
    </div>`;
  }

  function applySettingsFilter() {
    const q = settingsQuery.trim().toLowerCase();
    const rows = document.querySelectorAll("#settings-page .setting");
    let visible = 0;
    rows.forEach((row) => {
      const hit = !q || (row.getAttribute("data-search") || "").indexOf(q) >= 0;
      row.hidden = !hit;
      if (hit) visible++;
    });
    document.querySelectorAll("#settings-page .settings-group").forEach((g) => {
      const any = g.querySelectorAll(".setting:not([hidden])").length > 0;
      g.hidden = !any;
    });
    const none = document.getElementById("settings-none");
    if (none) none.hidden = visible > 0;
  }

  function agentRows(agents) {
    return (agents || []).map((a) => {
      const prog = ctxFill(a) || 0;
      const escalated = a.escalatedFrom ? `<div class="agent-escalated">escalated from ${esc(a.escalatedFrom)}</div>` : "";
      const tokens = ctxLabel(a) ? " · context " + ctxLabel(a) : "";
      const kids = (agents || []).filter((x) => x.parentId === a.id);
      return `<div class="agent-row">
        <span class="codicon ${statusIconCodicon(a.status)}" style="color:${statusColor(a.status)}" aria-hidden="true"></span>
        <div class="agent-main">
          <div class="agent-top"><span class="agent-name">${esc(a.title || a.role)}</span> <span class="agent-role">${esc(titleCase(a.role))}</span></div>
          <div class="agent-model">${esc(a.model || "")}${esc(tokens)}</div>
          ${escalated}
          <div class="agent-track"><div class="agent-bar" style="width:${prog}%"></div></div>
        </div>
      </div>` + (kids.length ? `<div class="agent-children">${agentRows(kids)}</div>` : "");
    }).join("");
  }

  // Expected shape (protocol does not supply it yet):
  //   state.efficiency = { preset: string, profiles?: { [agentIdOrRole: string]: string } }
  // Absent or non-object -> renders nothing.
  function efficiencyHtml(eff) {
    if (!eff || typeof eff !== "object" || Array.isArray(eff)) return "";
    const profiles = eff.profiles && typeof eff.profiles === "object" && !Array.isArray(eff.profiles) ? eff.profiles : {};
    const rows = Object.keys(profiles)
      .filter((k) => typeof profiles[k] === "string")
      .map((k) => `<tr><th scope="row">${esc(k)}</th><td>${esc(profiles[k])}</td></tr>`)
      .join("");
    const preset = typeof eff.preset === "string" ? eff.preset : "";
    return `<div class="section-header" role="button" tabindex="0" aria-expanded="${effOpen ? "true" : "false"}" data-toggle="efficiency"><span class="codicon ${effOpen ? "codicon-chevron-down" : "codicon-chevron-right"}"></span><span>Token saving</span></div>
        <div class="section-body" ${effOpen ? "" : "hidden"}><table class="eff-table"><tbody><tr><th scope="row">Saving level</th><td>${esc(titleCase(preset))}</td></tr>${rows}</tbody></table></div>`;
  }

  function msgHtml(m) {
    if (m.role === "system") {
      return `<div class="msg system" role="note">
      <div class="msg-text sys"><span class="codicon codicon-info" aria-hidden="true"></span><div>${md(m.text || "")}</div></div>
    </div>`;
    }
    if (m.role === "error") {
      return `<div class="msg error" role="alert">
      <div class="msg-text err"><span class="codicon codicon-error" aria-hidden="true"></span><div>${md(m.text || "Request failed")}</div></div>
    </div>`;
    }
    const who = m.role === "assistant" ? "Assistant" : (m.role === "user" ? "You" : esc(m.role));
    const stream = m.streaming ? `<span class="stream" aria-hidden="true"></span>` : "";
    return `<div class="msg ${m.role === "user" ? "user" : ""}"${m.id ? ` data-key="m-${attr(m.id)}"` : ""}>
      <div class="msg-role">${esc(who)}</div>
      <div class="msg-text">${md(m.text || "")}${stream}</div>
    </div>`;
  }

  function empty(s) {
    return `<div class="empty">${esc(s)}</div>`;
  }

  function primaryModelName() {
    const b = state.backend || {};
    if (b.primary) {
      const m = (b.models || []).find((x) => x.id === b.primary);
      return m ? m.name || m.id : b.primary;
    }
    return "Primary";
  }

  // Model id tail for the composer button ("openai/gpt-5" -> "gpt-5"); placeholder when unset.
  function modelTail() {
    const p = String((state.backend || {}).primary || "");
    return p ? p.split("/").pop() : "Select model";
  }

  function statusCls() {
    const b = state.backend || {};
    if (!b.ready) return "bad";
    if (activeAgents().length) return "busy";
    return "";
  }

  function activeAgents() {
    return (state.agents || []).filter((a) => a.status === "running" || a.status === "starting");
  }

  function statusIconCodicon(status) {
    switch (status) {
      case "completed": return "codicon-check";
      case "running":
      case "starting": return "codicon-loading codicon-modifier-spin";
      case "failed": return "codicon-error";
      case "cancelled": return "codicon-circle-slash";
      case "waiting": return "codicon-debug-pause";
      case "blocked": return "codicon-debug-pause";
      case "queued": return "codicon-circle-outline";
      default: return "codicon-circle-outline";
    }
  }

  function statusColor(status) {
    switch (status) {
      case "completed": return "var(--vscode-charts-green)";
      case "running":
      case "starting": return "var(--vscode-charts-blue)";
      case "failed": return "var(--vscode-charts-red)";
      case "waiting":
      case "blocked": return "var(--vscode-charts-yellow)";
      default: return "var(--vscode-descriptionForeground)";
    }
  }

  function dashboard() {
    const agents = state.agents || [];
    const tasks = state.tasks || [];
    const msgs = state.messages || [];
    const changes = (state.changes || []).length;
    const verification = (state.verification || []).length;
    const mode = currentMode();
    const tokens = agents.reduce((n, a) => n + (a.usedTokens || 0), 0);
    return `<header class="header">
      <span class="status-dot ${statusCls()}"></span>
      <span class="header-title">Relo Code</span>
      <span class="header-meta">${esc(statusLabel())}</span>
      ${agents.length ? `<span class="header-spacer"></span><span class="header-meta header-counts" title="${esc(plural(agents.length, "agent") + " · " + plural(tasks.length, "task") + " · " + fmtTokens(tokens) + " tokens used in total, counting every model call")}">${esc(plural(agents.length, "agent"))} · ${esc(plural(tasks.length, "task"))} · ${esc(fmtTokens(tokens))} tokens</span>` : ""}
    </header>
    <div class="dashboard-grid">
      <div class="col">
        <div class="section-header" role="button" tabindex="0" aria-expanded="true"><span class="codicon codicon-chevron-down"></span><span>Agents</span></div>
        <div class="section-body">${agentRows(agents.filter((a) => !a.parentId)) || empty("No agents")}</div>
        <div class="section-header" role="button" tabindex="0" aria-expanded="true"><span class="codicon codicon-chevron-down"></span><span>Tasks</span></div>
        <div class="section-body">${tasks.map((t) => `<div class="task-row">${mark(t.status)} ${esc(t.title)} <span class="task-meta">${esc(t.status)}${t.model ? " · " + esc(t.model) : ""}</span></div>`).join("") || empty("No tasks")}</div>
        ${efficiencyHtml(state.efficiency)}
      </div>
      <div class="col">
        ${msgs.map(msgHtml).join("") || empty("Conversation will appear here.")}
        <div class="composer">
          <textarea id="box" rows="1" placeholder="Ask Relo…"></textarea>
          <div class="toolbar">
            <div class="toolbar-left">${modeSelect(mode)}</div>
            <div class="toolbar-right">
              <button type="button" id="send" class="send" aria-label="${isRunning(state) ? "Stop" : "Send message"}"><span class="codicon ${isRunning(state) ? "codicon-debug-stop" : "codicon-send"}"></span></button>
            </div>
          </div>
        </div>
      </div>
      <div class="col">
        <div class="section-header"><span class="codicon codicon-chevron-down"></span><span>Cache</span></div>
        <div class="cache-line">${esc(formatCache(state).join("  "))}</div>
      </div>
      <div class="bar">${esc("Context " + fmtTokens(tokens))} <span>${esc(plural(changes, "file"))} changed</span> <span>Verification ${verification}</span></div>
    </div>`;
  }

  function statusLabel() {
    const b = state.backend || {};
    if (!b.ready) return b.error ? "Failed" : "Starting";
    if (activeAgents().length) return plural(activeAgents().length, "agent");
    return "Ready";
  }

  function inspector() {
    const ins = state.inspection || {};
    const included = ins.included || ins.Included || [];
    const excluded = ins.excluded || ins.Excluded || [];
    const comp = ins.composition || ins.Composition || {};
    const used = ins.usedTokens || ins.Used || 0;
    const budget = ins.budget || ins.Budget || 0;
    const bars = Object.keys(comp).map((k, i) => {
      const colors = ["var(--vscode-charts-blue)", "var(--vscode-charts-green)", "var(--vscode-charts-orange)", "var(--vscode-charts-purple)", "var(--vscode-charts-yellow)", "var(--vscode-charts-red)"];
      const pct = used ? (100 * (comp[k] || 0) / used) : 0;
      return `<span title="${esc(k)}" style="width:${pct}%;background:${colors[i % colors.length]}"></span>`;
    }).join("");
    return `<header class="header"><span class="header-title">Context</span><span class="header-meta">generation ${esc(ins.generation || ins.Generation || 0)} · ${esc(fmtTokens(used))} / ${esc(fmtTokens(budget))}</span></header>
      <main class="inspector">
        <div class="bar-seg">${bars || ""}</div>
        <p class="meta">${esc(ins.refreshReason || ins.Reason || "")}</p>
        <div class="section-header"><span class="codicon codicon-chevron-right"></span><span>Included</span></div>
        <ul>${included.map((it) => `<li>${esc(it.title || it.Title || it.class || it.Class)} <span class="meta">${esc(it.source || it.Source || "")} ${esc(fmtTokens(it.tokens || it.Tokens || 0))}</span></li>`).join("") || empty("Nothing included")}</ul>
        <div class="section-header"><span class="codicon codicon-chevron-right"></span><span>Excluded</span></div>
        <ul>${excluded.map((it) => `<li>${esc(it.title || it.Title || it.class || it.Class)} <span class="meta">${esc(it.reason || "")}</span></li>`).join("") || empty("Nothing excluded")}</ul>
      </main>`;
  }

  function mark(status) {
    const color = (c) => `style="color:${c}"`;
    if (status === "completed") return `<span class="codicon codicon-check" ${color("var(--vscode-charts-green)")} aria-hidden="true"></span>`;
    if (status === "running" || status === "starting") return `<span class="codicon codicon-loading codicon-modifier-spin" ${color("var(--vscode-charts-blue)")} aria-hidden="true"></span>`;
    if (status === "failed") return `<span class="codicon codicon-error" ${color("var(--vscode-charts-red)")} aria-hidden="true"></span>`;
    if (status === "waiting" || status === "blocked") return `<span class="codicon codicon-debug-pause" ${color("var(--vscode-charts-yellow)")} aria-hidden="true"></span>`;
    if (status === "queued") return `<span class="codicon codicon-circle-outline" ${color("var(--vscode-descriptionForeground)")} aria-hidden="true"></span>`;
    return `<span class="codicon codicon-circle-small-filled" ${color("var(--vscode-descriptionForeground)")} aria-hidden="true"></span>`;
  }

  function sel(a, b) { return a === b ? " selected" : ""; }

  // Test exports: pure renderers and helpers. Declared before the browser guard so node tests can run them.
  if (typeof module !== "undefined") {
    module.exports = {
      submit, md, handleKeyDown, handleKey: handleKeyDown, handleKeydown: handleKeyDown, handleComposerKeydown: handleKeyDown, onKeyDown: handleKeyDown, onKeydown: handleKeyDown, keyDown: handleKeyDown, isRunning, running: isRunning, isRunningState: isRunning, markdown: md, renderMarkdown: md,
      fmtTokens, plural, fmtElapsed, enterAction, scrollAfterRender, runAgents, cardKind, phaseLabel,
      runPanelHtml: (s, now, ui) => withState(s, () => runPanelHtml(s, now, ui)),
      msgHtml: (m) => msgHtml(m),
      chatHtml: (s) => withState(s, chat),
      dashboardHtml: (s) => withState(s, dashboard),
      agentRowsHtml: (agents) => agentRows(agents),
      efficiencyHtml: (eff) => efficiencyHtml(eff),
      stripHtml: (s) => withState(s, () => strip(s)),
      drawerHtml: (name, s) => withState(s, () => drawerBody(name, s)),
      toolbarHtml: (s) => withState(s, () => toolbar(s)),
      chipCounts: (s) => chipCounts(s || {}),
      contextPct: (s) => contextPct(s || {}),
      settingsPageHtml: (cache, query) => withSettings(cache, query, settingsPage),
      settingsKeysOf: () => SETTINGS_GROUPS.map((g) => ({ id: g.id, keys: g.keys.slice() })),
      settingsRowHtml: (cache, key) => withSettings(cache, "", () => settingRow(key)),
      isModifiedSetting: (cache, key) => withSettings(cache, "", () => isModified(key)),
      settingFilterText: (key) => settingLabel(key),
      modelHint: (m) => modelHint(m),
      filterModels, defaultActive, navKeys, pickerRows, pickerListHtml, pickerOpenHtml, pickerClosedHtml, pickerCountText, cleanCollapsed,
      effectiveWindow: (cache, max) => withSettings(cache, "", () => effectiveWindow(max)),
      providerFormIssue: (f, existing) => withSettings({ values: { "relo.providers": existing || [] } }, "", () => providerFormIssue(f)),
      providerFromForm,
      slugify,
      orderedMenuCommands: () => MENU_COMMANDS.slice(),
      drawerNames: () => DRAWERS.slice(),
      messageForSetting: (key, value) => ({ type: "settings.set", key, value }),
      messageForReset: (key) => ({ type: "settings.reset", key }),
      historyHtml: (ui, now) => historyPage(ui, now),
      relTime, isDecided,
      messageForCommand: (command, id) => (id === undefined ? { type: "command", command } : { type: "command", command, id }),
    };
  }

  function withSettings(cache, query, fn) {
    const prevC = settingsCache;
    const prevQ = settingsQuery;
    settingsCache = cache || null;
    settingsQuery = query || "";
    try { return fn(); } finally { settingsCache = prevC; settingsQuery = prevQ; }
  }

  const MENU_COMMANDS = ["relo.pause", "relo.resume", "relo.stopAll", "relo.refreshPrimary", "relo.restartBackend", "relo.openLogs", "relo.showCacheStats"];
  const DRAWERS = ["agents", "tasks", "context", "changes", "checks"];

  if (typeof acquireVsCodeApi !== "function") return;

  const vscode = acquireVsCodeApi();
  const root = document.getElementById("app");
  const view = document.body.getAttribute("data-view") || "chat";
  const saved = vscode.getState() || {};
  state = saved.state || state;
  openDrawerName = saved.drawer || "";
  page = ["settings", "history"].includes(saved.page) ? saved.page : "chat";
  settingsQuery = saved.settingsQuery || "";
  settingsScroll = saved.settingsScroll || 0;
  composerDraft = typeof saved.draft === "string" ? saved.draft : "";
  const post = (message) => vscode.postMessage(message);

  // State is saved so a reloaded webview paints at once. Serialising it is not free, so writes are batched.
  let persistTimer = 0;
  function writeState() {
    const sp = document.getElementById("settings-page");
    vscode.setState({ state, drawer: openDrawerName, page, settingsQuery, settingsScroll: sp ? sp.scrollTop : settingsScroll, draft: composerDraft });
  }
  function persist() {
    if (persistTimer) return;
    persistTimer = setTimeout(() => { persistTimer = 0; writeState(); }, 500);
  }

  // Switches page. Entering a page that shows fetched data asks the extension for it.
  function goPage(name) {
    page = name === "settings" || name === "history" ? name : "chat";
    menuOpen = false;
    if (page === "settings") post({ type: "settings.get" });
    if (page === "history") post({ type: "sessions.list" });
    render();
  }

  window.addEventListener("message", (ev) => {
    const msg = ev.data;
    if (!msg) return;
    if (msg.type === "state") {
      const before = state.session && state.session.id;
      state = msg.state || state;
      if (before && state.session && state.session.id !== before) {
        // A different session: per-run UI state of the old one means nothing here.
        runUi.cardOpen = {};
        runUi.showAll = false;
        for (const k of Object.keys(checkOpen)) delete checkOpen[k];
      }
      // An open provider form keeps focus: streaming state must not rebuild the page under the cursor.
      if (page === "settings" && (providerForm || openPicker)) persist();
      else render();
    } else if (msg.type === "settings") {
      settingsCache = msg;
      if (!uiCollapsedLoaded) { uiCollapsed = cleanCollapsed(msg.collapsed); uiCollapsedLoaded = true; }
      render();
    } else if (msg.type === "provider.discovered") {
      onDiscovered(msg);
    } else if (msg.type === "sessions") {
      historyUi.sessions = Array.isArray(msg.sessions) ? msg.sessions : [];
      historyUi.error = typeof msg.error === "string" ? msg.error : "";
      if (page === "history") render();
    } else if (msg.type === "drawer") {
      openDrawerName = openDrawerName === msg.name ? "" : msg.name;
      if (openDrawerName) page = "chat";
      render();
    } else if (msg.type === "page") {
      goPage(msg.name);
    }
  });

  // --- Rendering -------------------------------------------------------------
  // The chat and history pages are patched in place. Replacing the whole DOM on every state event (several per
  // second while agents run) destroys the element under the pointer between mousedown and mouseup, so clicks were
  // lost, and it also reset scroll positions, focus and text selection.

  const keyOf = (n) => (n.nodeType === 1 ? n.getAttribute("data-key") || n.id || "" : "");
  const sameKind = (a, b) => a.nodeType === b.nodeType && (a.nodeType !== 1 || (a.tagName === b.tagName && keyOf(a) === keyOf(b)));

  function patchAttrs(a, b) {
    const keepStyle = a.tagName === "TEXTAREA"; // the composer sizes itself with an inline style
    for (const at of Array.from(a.attributes)) {
      if (!b.hasAttribute(at.name) && !(keepStyle && at.name === "style")) a.removeAttribute(at.name);
    }
    for (const at of Array.from(b.attributes)) {
      if (keepStyle && at.name === "style") continue;
      if (a.getAttribute(at.name) !== at.value) a.setAttribute(at.name, at.value);
    }
  }

  function patchNode(a, b) {
    if (a.nodeType !== 1) {
      if (a.nodeValue !== b.nodeValue) a.nodeValue = b.nodeValue;
      return;
    }
    patchAttrs(a, b);
    if (a.tagName === "TEXTAREA") return; // what the user typed is theirs
    const value = a.tagName === "SELECT" ? b.value : null;
    patchChildren(a, b);
    if (value !== null && document.activeElement !== a && a.value !== value) a.value = value;
  }

  // Makes the children of `from` match those of `to`, reusing nodes where it can. Elements with an id or data-key
  // are matched by it, everything else by position.
  function patchChildren(from, to) {
    let cur = from.firstChild;
    let want = to.firstChild;
    while (want) {
      const next = want.nextSibling;
      const key = keyOf(want);
      if (key && (!cur || keyOf(cur) !== key)) {
        let found = null;
        for (let n = cur; n; n = n.nextSibling) {
          if (n.nodeType === 1 && n.tagName === want.tagName && keyOf(n) === key) { found = n; break; }
        }
        if (found) {
          from.insertBefore(found, cur);
          patchNode(found, want);
          cur = found.nextSibling;
        } else {
          from.insertBefore(want, cur);
        }
      } else if (!cur) {
        from.appendChild(want);
      } else if (sameKind(cur, want)) {
        patchNode(cur, want);
        cur = cur.nextSibling;
      } else {
        const after = cur.nextSibling;
        from.replaceChild(want, cur);
        cur = after;
      }
      want = next;
    }
    while (cur) {
      const after = cur.nextSibling;
      from.removeChild(cur);
      cur = after;
    }
  }

  function morph(host, html) {
    const tpl = document.createElement("template");
    tpl.innerHTML = html;
    patchChildren(host, tpl.content);
  }

  // Puts the draft, caret/selection and messages scroll back after the DOM was patched or rebuilt.
  function restoreChatView(prev) {
    const box = document.getElementById("box");
    if (box) {
      const draft = prev.draft !== null && prev.draft !== "" ? prev.draft : composerDraft;
      if (box.value !== draft) box.value = draft;
      if (prev.focused && document.activeElement !== box) {
        box.focus();
        box.setSelectionRange(prev.selStart, prev.selEnd);
      }
      autosize();
    }
    const m = document.querySelector(".messages");
    if (m) m.scrollTop = scrollAfterRender(prev, (state.messages || []).length, m.scrollHeight);
  }

  // 1s ticker: only text nodes change, the DOM is never rebuilt for time.
  function tickRun() {
    const now = Date.now();
    document.querySelectorAll("[data-elapsed-from]").forEach((el) => {
      const from = Number(el.getAttribute("data-elapsed-from"));
      const t = fmtElapsed(now - from);
      if (el.textContent !== t) el.textContent = t;
    });
  }

  let lastKind = "";
  function render() {
    rendering = true; // rebuilding the DOM must not look like the user leaving an open picker
    const kind = view !== "chat" ? view : page;
    const inChat = kind === "chat";
    const prevView = inChat && lastKind === "chat" ? captureView() : null;
    let html;
    if (view === "dashboard") html = dashboard();
    else if (view === "context") html = inspector();
    else if (page === "settings") html = toolbar(state) + settingsPage();
    else if (page === "history") html = toolbar(state) + historyPage(historyUi, Date.now());
    else html = chat();
    if (kind === lastKind && (kind === "chat" || kind === "history")) morph(root, html);
    else root.innerHTML = html;
    lastKind = kind;
    rendering = false;
    if (inChat) restoreChatView(prevView || { draft: null, selStart: 0, selEnd: 0, focused: false, scrollTop: 0, nearBottom: true, msgCount: 0 });
    if (inChat) renderedMsgCount = (state.messages || []).length;
    else autosize();
    if (page === "settings" && view === "chat") {
      bindSettings();
      const sp = document.getElementById("settings-page");
      if (sp) sp.scrollTop = settingsScroll;
      applySettingsFilter();
      if (openPicker) {
        const q = document.getElementById(openPicker.id + "-q");
        if (q) { q.focus(); q.setSelectionRange(q.value.length, q.value.length); }
      }
    }
    persist();
  }

  // --- Events ----------------------------------------------------------------
  // One listener per event type, on the root, so it keeps working however the DOM underneath is patched. Every
  // command goes through the extension, which validates it against an allow-list.

  const menuEl = () => document.getElementById("more-menu");
  const menuItems = () => Array.from((menuEl() || document.createElement("div")).querySelectorAll("[role='menuitem']"));
  function focusMenuItem(i) {
    const list = menuItems();
    if (list.length) list[(i + list.length) % list.length].focus();
  }
  function setMenu(open, returnFocus) {
    const menu = menuEl();
    const more = document.getElementById("more");
    menuOpen = open;
    if (menu) menu.hidden = !open;
    if (more) {
      more.setAttribute("aria-expanded", open ? "true" : "false");
      if (!open && returnFocus) more.focus();
    }
    if (open) focusMenuItem(0);
  }

  function copyCode(btn) {
    const text = btn.getAttribute("data-code") || "";
    try {
      if (navigator.clipboard) navigator.clipboard.writeText(text);
      else {
        const ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
      }
    } catch (_e) {}
    const icon = btn.querySelector(".codicon");
    if (!icon) return;
    icon.className = "codicon codicon-check";
    clearTimeout(btn._reset);
    btn._reset = setTimeout(() => { icon.className = "codicon codicon-copy"; }, 1500);
  }

  function flipEfficiency() {
    effOpen = !effOpen;
    render();
  }

  function onRootClick(e) {
    const t = e.target;
    if (!t || !t.closest) return;
    const at = (selector) => t.closest(selector);
    let el;
    if (menuOpen && !at("#more-menu") && !at("#more")) setMenu(false, false); // a click elsewhere closes the menu
    if ((el = at("[data-menu-cmd]"))) { setMenu(false, false); post({ type: "command", command: el.getAttribute("data-menu-cmd") }); return; }
    if (at("#more")) { setMenu(!menuOpen, true); return; }
    if ((el = at("[data-cmd]"))) { post({ type: "command", command: el.getAttribute("data-cmd") }); return; }
    if ((el = at("[data-command]"))) { post({ type: "command", command: el.getAttribute("data-command"), id: el.getAttribute("data-id") }); return; }
    if ((el = at("[data-toggle-check]"))) {
      const i = el.getAttribute("data-toggle-check");
      checkOpen[i] = !checkOpen[i];
      render();
      return;
    }
    if ((el = at("[data-drawer]"))) {
      const name = el.getAttribute("data-drawer");
      openDrawerName = openDrawerName === name ? "" : name;
      render();
      return;
    }
    if ((el = at("[data-page]"))) { goPage(el.getAttribute("data-page")); return; }
    if (at("[data-settings-open]")) { post({ type: "settings.open" }); return; }
    if ((el = at("[data-session-open]"))) {
      if (el.getAttribute("data-current") === "1") goPage("chat");
      else post({ type: "session.open", id: el.getAttribute("data-session-open") });
      return;
    }
    if ((el = at("[data-session-delete]"))) { post({ type: "session.delete", id: el.getAttribute("data-session-delete") }); return; }
    if (at("#model")) { post({ type: "command", command: "relo.selectPrimaryModel" }); return; }
    if (at("#eff")) { goPage("settings"); return; }
    const send = at("#send");
    if (send) {
      const box = document.getElementById("box");
      if (isRunning(state)) post({ type: "cancel" });
      else if (box) { submit(box, post); composerDraft = ""; autosize(); }
      return;
    }
    if (at("[data-run-stop]")) { post({ type: "cancel" }); return; }
    if (at("[data-run-list]")) { runUi.listOpen = !runUi.listOpen; render(); return; }
    if (at("[data-run-more]")) { runUi.showAll = !runUi.showAll; render(); return; }
    if ((el = at("[data-run-card]"))) {
      const id = el.getAttribute("data-run-card");
      runUi.cardOpen[id] = !runUi.cardOpen[id];
      render();
      return;
    }
    if (at("[data-toggle='efficiency']")) { flipEfficiency(); return; }
    if ((el = at(".code-copy"))) copyCode(el);
  }

  function onRootKeydown(e) {
    const t = e.target;
    if (!t || !t.closest) return;
    if (t.id === "box") {
      const act = enterAction(e, isRunning(state));
      if (act === "ignore") return;
      e.preventDefault();
      if (act === "send") { submit(t, post); composerDraft = ""; autosize(); }
      return;
    }
    if (t.id === "more") {
      if (e.key === "ArrowDown" || e.key === "Enter" || e.key === " ") { e.preventDefault(); setMenu(true, false); }
      return;
    }
    if (t.closest("#more-menu")) {
      const list = menuItems();
      const i = list.indexOf(document.activeElement);
      if (e.key === "ArrowDown") { e.preventDefault(); focusMenuItem(i + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); focusMenuItem(i - 1); }
      else if (e.key === "Home") { e.preventDefault(); focusMenuItem(0); }
      else if (e.key === "End") { e.preventDefault(); focusMenuItem(list.length - 1); }
      else if (e.key === "Escape") { e.preventDefault(); setMenu(false, true); }
      else if (e.key === "Tab") setMenu(false, false);
      else if (e.key === "Enter" || e.key === " ") { e.preventDefault(); if (i >= 0) list[i].click(); }
      return;
    }
    if (t.closest("[data-toggle='efficiency']") && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); flipEfficiency(); }
  }

  root.addEventListener("click", onRootClick);
  root.addEventListener("keydown", onRootKeydown);
  root.addEventListener("input", (e) => {
    if (e.target && e.target.id === "box") { composerDraft = e.target.value; autosize(); persist(); }
  });
  root.addEventListener("change", (e) => {
    if (e.target && e.target.id === "mode") post({ type: "mode", mode: e.target.value });
  });

  function bindSettings() {
    const search = document.getElementById("settings-search");
    if (search) {
      search.addEventListener("input", () => {
        settingsQuery = search.value;
        applySettingsFilter();
        persist();
      });
    }
    document.querySelectorAll("[data-collapse]").forEach((h) => {
      const flip = () => {
        const id = h.getAttribute("data-collapse");
        setCollapsed("g:" + id, !isCollapsed("g:" + id));
        render();
        const again = document.querySelector(`[data-collapse="${id}"]`);
        if (again) again.focus();
      };
      h.addEventListener("click", flip);
      h.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); flip(); } });
    });
    document.querySelectorAll("[data-reset]").forEach((btn) => {
      btn.addEventListener("click", () => vscode.postMessage({ type: "settings.reset", key: btn.getAttribute("data-reset") }));
    });
    // Debounced text/number writes; select/checkbox write immediately.
    const timers = {};
    document.querySelectorAll("#settings-page [data-key]").forEach((el) => {
      const key = el.getAttribute("data-key");
      const level = el.getAttribute("data-level");
      const send = (value) => {
        if (level) {
          const map = Object.assign({}, valueOf(key) || {});
          if (value) map[level] = value; else delete map[level];
          vscode.postMessage({ type: "settings.set", key, value: map });
          return;
        }
        vscode.postMessage({ type: "settings.set", key, value });
      };
      const readValue = () => {
        if (el.type === "checkbox") return el.checked;
        if (el.getAttribute("data-type") === "number") return el.value === "" ? undefined : Number(el.value);
        if (el.tagName === "SELECT" && el.value === "" && !level) return "";
        return el.value;
      };
      if (el.type === "checkbox" || el.tagName === "SELECT") {
        el.addEventListener("change", () => { const v = readValue(); if (v !== undefined) send(v); });
      } else {
        el.addEventListener("input", () => {
          clearTimeout(timers[key]);
          timers[key] = setTimeout(() => {
            const v = readValue();
            if (v !== undefined) send(v);
          }, 400);
        });
      }
    });
    document.querySelectorAll("[data-mode]").forEach((btn) => {
      const pick = () => vscode.postMessage({ type: "settings.set", key: "relo.routing.mode", value: btn.getAttribute("data-mode") });
      btn.addEventListener("click", pick);
      btn.addEventListener("keydown", (e) => {
        if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
        e.preventDefault();
        const other = document.querySelector(`[data-mode]:not([data-mode="${btn.getAttribute("data-mode")}"])`);
        if (other) { other.focus(); other.click(); }
      });
    });
    bindProviders();
    bindPickers();
    const sp = document.getElementById("settings-page");
    if (sp) sp.addEventListener("scroll", () => { settingsScroll = sp.scrollTop; persist(); });
  }

  function pickerData(host) {
    const key = host.getAttribute("data-mkey");
    const level = host.getAttribute("data-mlevel") || "";
    const raw = valueOf(key);
    const current = level ? (raw && typeof raw === "object" && typeof raw[level] === "string" ? raw[level] : "") : (typeof raw === "string" ? raw : "");
    return { key, level, current, blank: host.getAttribute("data-blank") || "(none)", models: (settingsCache && settingsCache.models) || [] };
  }

  function closePicker(returnFocus) {
    const id = openPicker && openPicker.id;
    openPicker = null;
    render();
    if (returnFocus && id) { const b = document.getElementById(id + "-btn"); if (b) b.focus(); }
  }

  function chooseModel(d, ref) {
    // Optimistic: show the pick now; the settings round trip confirms it.
    let value = ref;
    if (d.level) {
      const map = Object.assign({}, valueOf(d.key) || {});
      if (ref) map[d.level] = ref; else delete map[d.level];
      value = map;
    }
    if (settingsCache && settingsCache.values) settingsCache.values[d.key] = value;
    vscode.postMessage({ type: "settings.set", key: d.key, value });
    closePicker(true);
  }

  function bindPickers() {
    document.querySelectorAll(".mpick").forEach((host) => {
      const pid = host.getAttribute("data-pid");
      const btn = host.querySelector(".mpick-btn");
      if (btn) {
        const open = () => { openPicker = { id: pid, query: "", active: "" }; render(); };
        btn.addEventListener("click", open);
        btn.addEventListener("keydown", (e) => { if (e.key === "ArrowDown") { e.preventDefault(); open(); } });
        return;
      }
      const q = host.querySelector(".mpick-q");
      const list = host.querySelector(".mpick-list");
      if (!q || !list || !openPicker) return;
      const rowsNow = () => { const d = pickerData(host); return Object.assign({ d }, pickerRows(d.models, openPicker.query, d.current, d.blank, uiCollapsed)); };
      const refresh = () => {
        const r = rowsNow();
        if (!r.rows.some((x) => x.key === openPicker.active)) openPicker.active = defaultActive(r, r.d.current);
        list.innerHTML = pickerListHtml(pid, r.rows, r.d.current, openPicker.active);
        const i = r.rows.findIndex((x) => x.key === openPicker.active);
        if (i >= 0) { q.setAttribute("aria-activedescendant", `${pid}-r${i}`); const el = document.getElementById(`${pid}-r${i}`); if (el && el.scrollIntoView) el.scrollIntoView({ block: "nearest" }); }
        else q.removeAttribute("aria-activedescendant");
        const c = document.getElementById(pid + "-count");
        if (c) c.textContent = pickerCountText(r, r.d.models.length);
      };
      const activate = (key) => {
        const r = rowsNow();
        const row = r.rows.find((x) => x.key === key);
        if (!row) return;
        if (row.kind === "group") {
          if (!row.toggle) return;
          setCollapsed("p:" + row.provider, !isCollapsed("p:" + row.provider));
          refresh();
        } else {
          chooseModel(r.d, row.ref);
        }
      };
      refresh();
      q.addEventListener("input", () => { openPicker.query = q.value; openPicker.active = ""; refresh(); });
      q.addEventListener("keydown", (e) => {
        if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closePicker(true); return; }
        if (e.key === "ArrowDown" || e.key === "ArrowUp") {
          e.preventDefault();
          const keys = navKeys(rowsNow());
          const i = Math.max(0, keys.indexOf(openPicker.active)) + (e.key === "ArrowDown" ? 1 : -1);
          if (keys.length) openPicker.active = keys[Math.min(keys.length - 1, Math.max(0, i))];
          refresh();
        } else if (e.key === "Enter") {
          e.preventDefault();
          activate(openPicker.active);
        }
      });
      list.addEventListener("mousedown", (e) => e.preventDefault()); // keep focus in the search box
      list.addEventListener("click", (e) => {
        const li = e.target.closest && e.target.closest("[data-rowkey]");
        if (li) activate(li.getAttribute("data-rowkey"));
      });
      host.addEventListener("focusout", (e) => {
        if (rendering || (e.relatedTarget && host.contains(e.relatedTarget))) return;
        closePicker(false);
      });
    });
  }

  // Provider form state lives in module variables, not the DOM, so a settings refresh never drops a draft.
  function refreshFormChrome() {
    const f = providerForm;
    if (!f) return;
    const issue = providerFormIssue(f);
    const save = document.getElementById("pf-save");
    if (save) save.disabled = !!issue;
    const host = document.querySelector(".pf-issue");
    const text = issue && (f.id || f.baseURL) ? issue : "";
    if (host) { host.textContent = text; host.hidden = !text; }
    else if (text) {
      const div = document.createElement("div");
      div.className = "mini pf-issue";
      div.textContent = text;
      const anchor = document.getElementById("pf-addmodel");
      if (anchor && anchor.parentNode) anchor.parentNode.insertBefore(div, anchor.nextSibling);
    }
  }

  function bindProviders() {
    const add = document.getElementById("prov-add");
    if (add) add.addEventListener("click", () => { providerForm = newProviderForm(); providerKeyDraft = ""; render(); const n = document.getElementById("pf-name"); if (n) n.focus(); });
    document.querySelectorAll("[data-prov-edit]").forEach((b) => b.addEventListener("click", () => {
      const p = providerList().find((x) => x.id === b.getAttribute("data-prov-edit"));
      if (!p) return;
      providerForm = formFromProvider(p);
      providerKeyDraft = "";
      render();
      const n = document.getElementById("pf-name");
      if (n) n.focus();
    }));
    document.querySelectorAll("[data-prov-remove]").forEach((b) => b.addEventListener("click", () => { providerRemoveConfirm = b.getAttribute("data-prov-remove"); render(); const y = document.querySelector("[data-prov-remove-yes]"); if (y) y.focus(); }));
    document.querySelectorAll("[data-prov-remove-no]").forEach((b) => b.addEventListener("click", () => { providerRemoveConfirm = ""; render(); }));
    document.querySelectorAll("[data-prov-remove-yes]").forEach((b) => b.addEventListener("click", () => {
      const id = b.getAttribute("data-prov-remove-yes");
      providerRemoveConfirm = "";
      if (providerForm && providerForm.editing === id) providerForm = null;
      vscode.postMessage({ type: "provider.remove", id });
      render();
    }));

    const f = providerForm;
    if (!f) return;
    const keyEl = document.getElementById("pf-key");
    if (keyEl) {
      keyEl.value = providerKeyDraft; // property, not attribute: the key never enters markup or saved state
      keyEl.addEventListener("input", () => { providerKeyDraft = keyEl.value; });
    }
    document.querySelectorAll("[data-pf]").forEach((el) => {
      const field = el.getAttribute("data-pf");
      const onChange = () => {
        f[field] = el.value;
        if (field === "name" && !f.idTouched && !f.editing) {
          f.id = slugify(el.value);
          const idEl = document.getElementById("pf-id");
          if (idEl) idEl.value = f.id;
        }
        if (field === "id") f.idTouched = true;
        refreshFormChrome();
      };
      el.addEventListener("input", onChange);
      el.addEventListener("change", onChange);
    });
    document.querySelectorAll("[data-pm-field]").forEach((el) => {
      el.addEventListener("input", () => {
        const m = f.models[Number(el.getAttribute("data-pm-index"))];
        if (m) m[el.getAttribute("data-pm-field")] = el.value;
        refreshFormChrome();
      });
    });
    document.querySelectorAll("[data-pm-remove]").forEach((b) => b.addEventListener("click", () => { f.models.splice(Number(b.getAttribute("data-pm-remove")), 1); render(); }));
    const all = document.querySelector("[data-pm-all]");
    if (all) all.addEventListener("click", () => { providerShowAll = true; render(); });
    const addModel = document.getElementById("pf-addmodel");
    if (addModel) addModel.addEventListener("click", () => {
      if (f.models.length >= 200) return;
      f.models.push({ id: "", name: "", context: "", output: "", efforts: "" });
      providerShowAll = true;
      render();
      const el = document.getElementById("pm-id-" + (f.models.length - 1));
      if (el) el.focus();
    });
    const fetchBtn = document.getElementById("pf-fetch");
    if (fetchBtn) fetchBtn.addEventListener("click", () => {
      f.error = "";
      f.note = "";
      try {
        const u = new URL(f.baseURL.trim());
        if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error("scheme");
      } catch (_e) {
        f.error = "Enter a valid http(s) Base URL first.";
        render();
        return;
      }
      f.busy = true;
      f.requestId = "d" + Date.now().toString(36);
      const msg = { type: "provider.discover", kind: f.kind, baseURL: f.baseURL.trim(), requestId: f.requestId };
      if (providerKeyDraft) msg.apiKey = providerKeyDraft;
      else if (f.editing) msg.id = f.editing;
      vscode.postMessage(msg);
      render();
    });
    const save = document.getElementById("pf-save");
    if (save) save.addEventListener("click", () => {
      if (providerFormIssue(f)) return;
      const msg = { type: "provider.save", provider: providerFromForm(f) };
      if (providerKeyDraft) msg.apiKey = providerKeyDraft;
      vscode.postMessage(msg);
      providerForm = null;
      providerKeyDraft = "";
      providerShowAll = false;
      render();
    });
    const cancel = document.getElementById("pf-cancel");
    if (cancel) cancel.addEventListener("click", () => { providerForm = null; providerKeyDraft = ""; providerShowAll = false; render(); });
  }

  function onDiscovered(msg) {
    const f = providerForm;
    if (!f || !f.requestId || msg.requestId !== f.requestId) return;
    f.busy = false;
    f.requestId = "";
    f.error = typeof msg.error === "string" ? msg.error : "";
    const found = Array.isArray(msg.models) ? msg.models : [];
    let added = 0;
    for (const d of found) {
      if (!d || typeof d.id !== "string") continue;
      const have = f.models.find((m) => m.id === d.id);
      const efforts = Array.isArray(d.efforts) ? d.efforts.join(", ") : "";
      if (have) {
        if (!have.context && d.context) have.context = String(d.context);
        if (!have.output && d.output) have.output = String(d.output);
        if (!have.efforts && efforts) have.efforts = efforts;
      } else if (f.models.length < 200) {
        f.models.push({ id: d.id, name: typeof d.name === "string" && d.name !== d.id ? d.name : "", context: d.context ? String(d.context) : "", output: d.output ? String(d.output) : "", efforts });
        added++;
      }
    }
    f.note = f.error ? "" : found.length ? `Found ${plural(found.length, "model")}, ${added} new.` : "The endpoint listed no models. Add them by id.";
    render();
  }

  function autosize() {
    const box = document.getElementById("box");
    if (!box) return;
    box.style.height = "auto";
    const max = 160;
    box.style.height = Math.min(box.scrollHeight, max) + "px";
  }

  setInterval(tickRun, 1000);
  render();
  if (page === "history") post({ type: "sessions.list" });
  if (page === "settings") post({ type: "settings.get" });
})();
