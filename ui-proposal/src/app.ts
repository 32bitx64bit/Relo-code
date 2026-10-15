type TaskStatus = "done" | "running" | "queued" | "failed";
type GraphTask = { id: string; label: string; difficulty: string; status: TaskStatus; agent: string; deps: string[] };

const $ = <T extends Element>(selector: string, root: ParentNode = document): T => {
  const element = root.querySelector<T>(selector);
  if (!element) throw new Error(`Missing element: ${selector}`);
  return element;
};
const $$ = <T extends Element>(selector: string, root: ParentNode = document): T[] => Array.from(root.querySelectorAll<T>(selector));
const el = <K extends keyof HTMLElementTagNameMap>(tag: K, className?: string, text?: string): HTMLElementTagNameMap[K] => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
};
const icons: Record<string, string[]> = {
  search: ["M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14Z", "m16 16 5 5"], pause: ["M8 5v14", "M16 5v14"], play: ["m7 4 13 8-13 8z"], stop: ["M6 6h12v12H6z"],
  "chevron-right": ["m9 18 6-6-6-6"], "chevron-down": ["m6 9 6 6 6-6"], refresh: ["M20 7v5h-5", "M20 12a8 8 0 1 0-2.3-5.7L20 8"], send: ["m22 2-7 20-4-9-9-4z", "M22 2 11 13"],
  plus: ["M12 5v14", "M5 12h14"], close: ["m18 6-12 12", "m6 6 12 12"], file: ["M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z", "M14 2v6h6"], folder: ["M3 7a2 2 0 0 1 2-2h5l2 2h7a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
  "git-branch": ["M6 5v14", "M18 8a6 6 0 0 1-6 6H8", "M6 5a2 2 0 1 0 0-4 2 2 0 0 0 0 4z", "M6 23a2 2 0 1 0 0-4 2 2 0 0 0 0 4z", "M18 8a2 2 0 1 0 0-4 2 2 0 0 0 0 4z"], settings: ["M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8z", "m19.4 15 .1.1 1.4 1.1-1.4 2.4-1.7-.6a8 8 0 0 1-1.7 1L16 21H8l-.2-2a8 8 0 0 1-1.7-1l-1.7.6L3 16.2l1.4-1.1a7 7 0 0 1 0-2.1L3 12l1.4-2.4 1.7.6a8 8 0 0 1 1.7-1L8 7h8l.2 2a8 8 0 0 1 1.7 1l1.7-.6L21 11.8l-1.4 1.1a7 7 0 0 1-.2 2.1z"],
  copy: ["M8 8h12v12H8z", "M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3"], check: ["m5 12 4 4L19 6"], alert: ["m10.3 3.9-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.7-3.1l-8-14a2 2 0 0 0-3.4 0z", "M12 9v4", "M12 17h.01"], shield: ["M12 22s8-4 8-11V5l-8-3-8 3v6c0 7 8 11 8 11z", "m9 12 2 2 4-4"], zap: ["m13 2-3 9h7L8 22l3-9H4z"],
  layers: ["m12 2 9 5-9 5-9-5z", "m3 12 9 5 9-5", "m3 17 9 5 9-5"], cpu: ["M5 5h14v14H5z", "M9 9h6v6H9z", "M9 1v4m6-4v4M9 19v4m6-4v4M1 9h4m-4 6h4m14-6h4m-4 6h4"], gauge: ["M12 14 16 8", "M20.2 16a9 9 0 1 0-16.4 0z"], "trending-down": ["m22 17-8.5-8.5-5 5L2 7", "M16 17h6v-6"], scissors: ["M6 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6z", "M6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6z", "m8 8 12 12M8 16 20 4"], "arrow-up-right": ["M7 17 17 7", "M7 7h10v10"], "rotate-cw": ["M20 7v5h-5", "M20 12a8 8 0 1 1-2.3-5.7L20 8"]
};
function svgIcon(name: string): SVGSVGElement {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("width", "16"); svg.setAttribute("height", "16");
  svg.setAttribute("fill", "none"); svg.setAttribute("stroke", "currentColor"); svg.setAttribute("stroke-width", "1.7");
  svg.setAttribute("stroke-linecap", "round"); svg.setAttribute("stroke-linejoin", "round"); svg.setAttribute("aria-hidden", "true");
  for (const d of icons[name] ?? []) { const path = document.createElementNS("http://www.w3.org/2000/svg", "path"); path.setAttribute("d", d); svg.append(path); }
  return svg;
}
function renderIcons(root: ParentNode = document): void {
  $$<HTMLElement>("[data-icon]", root).forEach(node => node.replaceChildren(svgIcon(node.dataset.icon ?? "")));
}
const tasks: GraphTask[] = [
  { id: "inspect", label: "Probe Haiku", difficulty: "failed", status: "failed", agent: "probe-2", deps: [] },
  { id: "escalate", label: "Sonnet retry", difficulty: "attempt 2", status: "running", agent: "probe-2", deps: ["inspect"] },
  { id: "fix", label: "Fix dedupe", difficulty: "hard", status: "running", agent: "worker-1", deps: ["escalate"] },
  { id: "test", label: "Add test", difficulty: "medium", status: "queued", agent: "worker-1", deps: ["escalate"] },
  { id: "review", label: "Review patch", difficulty: "expert", status: "queued", agent: "reviewer", deps: ["fix", "test"] },
  { id: "verify", label: "Verify suite", difficulty: "medium", status: "queued", agent: "test", deps: ["review"] },
];
let demoTimer: number | undefined;
let demoPaused = false;
let demoRunning = false;
let demoStep = 0;
let previousFocus: HTMLElement | null = null;

function toast(message: string): void {
  const item = el("div", "toast", message);
  $("#toastStack").append(item);
  window.setTimeout(() => item.remove(), 2600);
}

function renderGraph(): void {
  const svg = $("#taskGraph") as unknown as SVGSVGElement;
  while (svg.firstChild) svg.removeChild(svg.firstChild);
  const level = new Map<string, number>();
  for (const task of tasks) level.set(task.id, task.deps.length ? Math.max(...task.deps.map(dep => (level.get(dep) ?? 0) + 1)) : 0);
  const maxLevel = Math.max(...level.values());
  const columns = Array.from({ length: maxLevel + 1 }, (_, index) => tasks.filter(task => level.get(task.id) === index));
  const width = 500;
  const height = Math.max(178, ...columns.map(column => column.length * 46 + 14));
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("preserveAspectRatio", "xMidYMid meet");
  const positions = new Map<string, { x: number; y: number }>();
  const nodeWidth = 90;
  const nodeHeight = 34;
  columns.forEach((column, col) => column.forEach((task, row) => {
    positions.set(task.id, { x: 8 + col * (nodeWidth + 6), y: 8 + row * 46 });
  }));
  const defs = document.createElementNS("http://www.w3.org/2000/svg", "defs");
  const marker = document.createElementNS("http://www.w3.org/2000/svg", "marker");
  marker.setAttribute("id", "arrowhead"); marker.setAttribute("markerWidth", "8"); marker.setAttribute("markerHeight", "8"); marker.setAttribute("viewBox", "0 0 8 8"); marker.setAttribute("refX", "7"); marker.setAttribute("refY", "4"); marker.setAttribute("orient", "auto");
  const arrow = document.createElementNS("http://www.w3.org/2000/svg", "path"); arrow.setAttribute("d", "M0 0 8 4 0 8z"); arrow.setAttribute("fill", "#edc879"); marker.append(arrow); defs.append(marker); svg.append(defs);
  for (const task of tasks) {
    const to = positions.get(task.id);
    if (!to) continue;
    for (const dependency of task.deps) {
      const from = positions.get(dependency);
      if (!from) continue;
      const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
      path.setAttribute("d", `M ${from.x + nodeWidth} ${from.y + nodeHeight / 2} C ${from.x + nodeWidth + 8} ${from.y + nodeHeight / 2}, ${to.x - 8} ${to.y + nodeHeight / 2}, ${to.x} ${to.y + nodeHeight / 2}`);
      const isEscalation = task.id === "escalate";
      path.setAttribute("class", `edge${tasks.find(item => item.id === dependency)?.status === "done" ? " done" : ""}${isEscalation ? " escalation-edge" : ""}`);
      if (isEscalation) {
        path.setAttribute("marker-end", "url(#arrowhead)");
      }
      svg.append(path);
    }
  }
  for (const task of tasks) {
    const point = positions.get(task.id);
    if (!point) continue;
    const group = document.createElementNS("http://www.w3.org/2000/svg", "g");
    group.setAttribute("class", `node ${task.status}`);
    group.setAttribute("transform", `translate(${point.x} ${point.y})`);
    group.setAttribute("tabindex", "0");
    group.setAttribute("role", "button");
    group.setAttribute("aria-label", `${task.label}, ${task.difficulty}, ${task.status}, agent ${task.agent}`);
    group.dataset.agent = task.agent;
    const rect = document.createElementNS("http://www.w3.org/2000/svg", "rect");
    rect.setAttribute("width", String(nodeWidth)); rect.setAttribute("height", String(nodeHeight)); rect.setAttribute("rx", "6");
    const dot = document.createElementNS("http://www.w3.org/2000/svg", "circle");
    dot.setAttribute("cx", "10"); dot.setAttribute("cy", "11"); dot.setAttribute("r", "3");
    const title = document.createElementNS("http://www.w3.org/2000/svg", "text");
    title.setAttribute("x", "19"); title.setAttribute("y", "14"); title.textContent = task.label;
    const subtitle = document.createElementNS("http://www.w3.org/2000/svg", "text");
    subtitle.setAttribute("x", "7"); subtitle.setAttribute("y", "27"); subtitle.setAttribute("class", "sub"); subtitle.textContent = `${task.difficulty} · ${task.agent}`;
    group.append(rect, dot, title, subtitle);
    group.addEventListener("click", () => focusAgent(task.agent));
    group.addEventListener("keydown", event => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); focusAgent(task.agent); } });
      svg.append(group);
  }
}

function focusAgent(agent: string): void {
  const row = $<HTMLElement>(`[data-agent="${CSS.escape(agent)}"]`, $("#agentTree"));
  row.scrollIntoView({ block: "nearest", behavior: "smooth" });
  row.focus();
  row.style.background = "var(--accent-soft)";
  window.setTimeout(() => { row.style.background = ""; }, 900);
}

function setTab(name: string): void {
  $$(".side-tab").forEach(tab => {
    const active = (tab as HTMLElement).dataset.view === name;
    tab.classList.toggle("active", active);
    tab.setAttribute("aria-selected", String(active));
    (tab as HTMLButtonElement).tabIndex = active ? 0 : -1;
  });
  $$(".view").forEach(view => {
    const active = view.id === `view-${name}`;
    view.classList.toggle("active", active);
    view.setAttribute("aria-hidden", String(!active));
    (view as HTMLElement).tabIndex = active ? 0 : -1;
  });
}

function setTheme(): void {
  const root = document.documentElement;
  const light = root.dataset.theme !== "light";
  if (light) root.dataset.theme = "light";
  else delete root.dataset.theme;
  $("#themeToggle").setAttribute("aria-label", light ? "Toggle dark theme" : "Toggle light theme");
  toast(`${light ? "Light" : "Dark"} theme`);
}

function setCacheRate(rate: number): void {
  const rounded = Math.min(99, Math.max(0, rate));
  $("#cachePercent").textContent = `${rounded}%`;
  $("#hitRateStat").textContent = `${rounded}%`;
  $("#statusHitRate").textContent = `Cache ${rounded}%`;
  $("#cacheGauge").setAttribute("style", `background:conic-gradient(var(--green) 0 ${rounded}%,var(--vscode-border) ${rounded}% 100%)`);
}

function selectPreset(preset: string): void {
  const savings: Record<string, number> = { off: 0, auto: 38, max: 53 };
  $$("[data-preset]").forEach(button => {
    const selected = (button as HTMLElement).dataset.preset === preset;
    button.classList.toggle("selected", selected);
    button.setAttribute("aria-pressed", String(selected));
  });
  $("#efficiencyChip").textContent = `Eff ${preset} −${savings[preset] ?? 0}% tok`;
}

function selectAnatomy(agent: string): void {
  const rows: Record<string, [string, string, string, string, string]> = {
    probe: ["180 tok", "~280 max · 210 tok", "1.2k tok", "2.8k tok", "1.6k tok"],
    worker: ["220 tok", "~280 max · 265 tok", "2.4k tok", "8.1k tok", "2.2k tok"],
    reviewer: ["190 tok", "~280 max · 270 tok", "1.8k tok", "5.2k tok", "1.1k tok"]
  };
  const values = rows[agent] ?? rows.probe;
  ["#tokensL0", "#tokensStyle", "#tokensL1", "#tokensL2", "#tokensL3"].forEach((selector, index) => { $(selector).textContent = values[index]; });
  const segments = $$(".anatomy-bar span");
  const widths: Record<string, number[]> = { probe: [5, 3, 26, 46, 20], worker: [3, 4, 19, 62, 12], reviewer: [4, 4, 24, 54, 18] };
  segments.forEach((segment, index) => (segment as HTMLElement).style.width = `${(widths[agent] ?? widths.probe)[index]}%`);
}

function selectMode(mode: string): void {
  const button = $("#modeButton");
  button.replaceChildren(document.createTextNode(`${mode} `), svgIcon("chevron-down"));
}

function setTask(id: string, status: TaskStatus): void {
  const task = tasks.find(item => item.id === id);
  if (task) task.status = status;
  renderGraph();
}

function demoTick(): void {
  if (demoPaused || demoTimer === undefined) return;
  const host = $("#demoMessages");
  if (demoStep === 0) {
    setTask("test", "running");
    $("#escalationEvent").removeAttribute("hidden");
    toast("probe-2 escalated to Sonnet · new session");
  } else if (demoStep === 1) {
    const message = el("article", "message");
    const meta = el("div", "message-meta", "●  worker-1 · Sonnet 4 · implementation");
    const bubble = el("div", "bubble", "In-flight refresh promise stays registered until persistence settles. Regression case now covers concurrent callers.");
    message.append(meta, bubble);
    host.append(message);
  } else if (demoStep === 2) {
    selectPreset("auto");
    const divider = el("div", "refresh");
    const icon = el("span"); icon.dataset.icon = "rotate-cw";
    const label = el("span", "", "Context refreshed · Generation 3 to 4 · preserved verified finding; dropped raw tool output");
    divider.append(icon, label); renderIcons(divider);
    host.append(divider);
    $("#contextItems").append(el("div", "context-item", "Generation 4 · latest test result included · raw history compressed"));
    toast("Worker context refreshed; session identity preserved");
    setCacheRate(89);
  } else if (demoStep === 3) {
    setTask("fix", "done"); setTask("test", "done"); setTask("review", "running");
    setCacheRate(92);
    toast("Worktree patch ready for independent review");
  } else if (demoStep === 4) {
    setTask("review", "done"); setTask("verify", "running");
    setCacheRate(95);
    toast("Cache hit rate reached 95% target");
  } else if (demoStep === 5) {
    setTask("verify", "done");
    const report = el("div", "report");
    const top = el("div", "report-top", "✓  Reviewer report · compressed for Primary");
    const body = el("p", "", "No race remains. Regression test confirms concurrent callers share one refresh rotation.");
    const foot = el("div", "report-foot", "5 tasks complete · auth suite passed · worktree awaits accept");
    report.append(top, body, foot); host.append(report);
    toast("Demo complete · worktree isolated; accept or reject patch");
    stopDemo();
    return;
  }
  demoStep++;
  $("#threadScroll").scrollTop = $("#threadScroll").scrollHeight;
  demoTimer = window.setTimeout(demoTick, 1350);
}

function stopDemo(): void {
  if (demoTimer !== undefined) window.clearTimeout(demoTimer);
  demoTimer = undefined;
  demoRunning = false;
  const button = $("#demoButton"); button.replaceChildren(svgIcon("play"), document.createTextNode(" Play demo"));
  $("#demoButton").classList.remove("demo-active");
}

function startDemo(): void {
  if (demoRunning) {
    demoPaused = !demoPaused;
    if (demoPaused && demoTimer !== undefined) window.clearTimeout(demoTimer);
    const button = $("#demoButton"); button.replaceChildren(svgIcon(demoPaused ? "play" : "pause"), document.createTextNode(demoPaused ? " Resume demo" : " Pause demo"));
    if (!demoPaused) demoTimer = window.setTimeout(demoTick, 500);
    return;
  }
  demoStep = 0; demoPaused = false;
  $("#escalationEvent").setAttribute("hidden", "");
  demoRunning = true;
  const button = $("#demoButton"); button.replaceChildren(svgIcon("pause"), document.createTextNode(" Pause demo"));
  $("#demoButton").classList.add("demo-active");
  toast("Live demo started · plan → build → review");
  demoTimer = window.setTimeout(demoTick, 650);
}

const commands = ["Switch to Plan mode", "Switch to Build mode", "Switch to Review mode", "Refresh Primary context", "Pause all agents", "Resume all agents", "Open Context inspector", "Open Changes and Verification", "Open Routing settings", "Open Efficiency panel", "Toggle light theme", "Play live demo"];
function openPalette(): void {
  previousFocus = document.activeElement as HTMLElement;
  $("#paletteBackdrop").classList.add("open");
  const input = $("#paletteInput") as HTMLInputElement;
  input.value = "";
  drawCommands("");
  input.focus();
}
function closePalette(): void {
  $("#paletteBackdrop").classList.remove("open");
  previousFocus?.focus();
}
function drawCommands(filter: string): void {
  const target = $("#paletteResults");
  while (target.firstChild) target.removeChild(target.firstChild);
  const terms = filter.toLocaleLowerCase().trim().split(/\s+/).filter(Boolean);
  const filtered = commands.map((label, order) => ({ label, order, score: terms.reduce((score, term) => score + (label.toLocaleLowerCase().includes(term) ? 1 : -20), 0) })).filter(item => item.score >= 0).sort((a, b) => b.score - a.score || a.order - b.order);
  if (!filtered.length) { target.append(el("div", "empty-state", "No matching commands")); return; }
  for (const command of filtered) {
    const button = el("button", "", command.label);
    button.setAttribute("role", "option");
    button.addEventListener("click", () => runCommand(command.label));
    target.append(button);
  }
}
function runCommand(command: string): void {
  closePalette();
  if (command.includes("Plan mode")) selectMode("Plan");
  else if (command.includes("Build mode")) selectMode("Build");
  else if (command.includes("Review mode")) selectMode("Review");
  else if (command.includes("Efficiency")) setTab("efficiency");
  else if (command.includes("Context inspector")) setTab("context");
  else if (command.includes("Changes")) setTab("changes");
  else if (command.includes("Routing")) setTab("routing");
  else if (command.includes("theme")) setTheme();
  else if (command.includes("demo")) startDemo();
  else if (command.includes("Refresh")) toast("Primary context refresh queued");
  else toast(command.includes("Pause") ? "Agents paused" : "Agents resumed");
  if (command.includes("mode")) toast(command.replace("Switch to ", "") + " mode selected");
}

function init(): void {
  if (new URLSearchParams(location.search).get("theme") === "light") document.documentElement.dataset.theme = "light";
  renderGraph();
  renderIcons();
  const initialView = new URLSearchParams(location.search).get("view");
  if (["overview", "context", "changes", "routing", "efficiency"].includes(initialView ?? "")) setTab(initialView!);
  $$(".side-tab").forEach(tab => tab.addEventListener("click", () => setTab((tab as HTMLElement).dataset.view ?? "overview")));
  $(".side-tabs").addEventListener("keydown", event => {
    const keyboardEvent = event as KeyboardEvent;
    const tabs = $$(".side-tab", keyboardEvent.currentTarget as ParentNode) as HTMLButtonElement[];
    const current = tabs.indexOf(document.activeElement as HTMLButtonElement);
    if (current < 0 || !["ArrowLeft", "ArrowRight", "Home", "End"].includes(keyboardEvent.key)) return;
    const nextIndex = keyboardEvent.key === "Home" ? 0 : keyboardEvent.key === "End" ? tabs.length - 1 : (current + (keyboardEvent.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
    keyboardEvent.preventDefault(); tabs[nextIndex].focus(); tabs[nextIndex].click();
  });
  const tabs = $$(".side-tab") as HTMLButtonElement[];
  tabs.forEach(tab => {
    tab.tabIndex = tab.getAttribute("aria-selected") === "true" ? 0 : -1;
    tab.setAttribute("aria-controls", `view-${tab.dataset.view}`);
  });
  setTab(initialView && ["overview", "context", "changes", "routing", "efficiency"].includes(initialView) ? initialView : "overview");
  $("#themeToggle").addEventListener("click", setTheme);
  $("#themeToggleSmall").addEventListener("click", setTheme);
  $("#demoButton").addEventListener("click", startDemo);
  $("#efficiencyChip").addEventListener("click", () => setTab("efficiency"));
  $$<HTMLElement>("[data-preset]").forEach(button => button.addEventListener("click", () => selectPreset(button.dataset.preset ?? "auto")));
  $("#anatomyAgent").addEventListener("change", event => selectAnatomy((event.currentTarget as HTMLSelectElement).value));
  $(".view#view-routing .toggle").addEventListener("click", event => {
    const toggle = event.currentTarget as HTMLButtonElement;
    toggle.setAttribute("aria-checked", String(toggle.getAttribute("aria-checked") !== "true"));
  });
  $(".view#view-efficiency .preset-switch").addEventListener("keydown", event => {
    const keyboardEvent = event as KeyboardEvent;
    if (!["ArrowLeft", "ArrowRight"].includes(keyboardEvent.key)) return;
    const buttons = $$("[data-preset]", keyboardEvent.currentTarget as ParentNode) as HTMLButtonElement[];
    const current = buttons.findIndex(button => button.getAttribute("aria-pressed") === "true");
    const next = buttons[(current + (keyboardEvent.key === "ArrowRight" ? 1 : -1) + buttons.length) % buttons.length];
    keyboardEvent.preventDefault(); next.focus(); next.click();
  });
  $("#pauseButton").addEventListener("click", event => {
    const button = event.currentTarget as HTMLButtonElement;
    const paused = button.getAttribute("aria-pressed") !== "true";
    button.setAttribute("aria-pressed", String(paused));
    button.replaceChildren(svgIcon(paused ? "play" : "pause"));
    toast(paused ? "Agents paused" : "Agents resumed");
  });
  $$(".tool-toggle").forEach(button => button.addEventListener("click", () => {
    const card = button.closest(".tool-card");
    if (!card) return;
    const collapsed = card.classList.toggle("collapsed");
    button.setAttribute("aria-expanded", String(!collapsed));
  }));
  $$("[data-hunk]").forEach(button => button.addEventListener("click", () => {
    const action = (button as HTMLElement).dataset.hunk;
    const diff = button.closest<HTMLElement>(".diff");
    if (diff) diff.style.opacity = action === "accept" ? ".55" : ".35";
    toast(action === "accept" ? "Hunk accepted into pending patch" : "Hunk rejected from pending patch");
  }));
  $$("[data-permission]").forEach(button => button.addEventListener("click", () => {
    const permission = (button as HTMLElement).dataset.permission ?? "Permission updated";
    button.closest(".permission")?.replaceChildren(el("div", "permission-title", `${permission} · pnpm test -- auth`));
    toast(`Permission: ${permission}`);
  }));
  $$("[data-item]").forEach(button => button.addEventListener("click", () => {
    const action = (button as HTMLElement).dataset.item ?? "updated";
    const item = button.closest(".context-item");
    if (action === "remove") item?.remove();
    else if (action === "promote" && item) item.querySelector(".item-reason")!.textContent = "Promoted to project memory · remains unverified until explicit verify";
    else if (action === "pin" && item) toast("Context item pinned; survives refresh");
    if (action !== "pin") toast(`Context item ${action === "promote" ? "promoted" : "removed"}`);
  }));
  $$("[data-patch]").forEach(button => button.addEventListener("click", () => {
    const accepted = (button as HTMLElement).dataset.patch === "accept";
    $("#changeEmpty").removeAttribute("hidden");
    toast(accepted ? "Worktree changes accepted" : "Worktree changes rejected");
  }));
  $$(".agent-action").forEach(button => button.addEventListener("click", event => {
    event.stopPropagation();
    const action = (button as HTMLElement).dataset.action;
    toast(action === "redirect" ? "Redirect prompt opened for Primary" : action === "stop" ? "Agent stop requested" : "Agent pause toggled");
  }));
  $$(".agent-row[data-agent]").forEach(row => row.addEventListener("click", () => {
    const agent = (row as HTMLElement).dataset.agent;
    if (agent) toast(`Focused ${agent}`);
  }));
  $$(".gen-btn").forEach(button => button.addEventListener("click", () => {
    $$(".gen-btn").forEach(item => item.classList.remove("active"));
    button.classList.add("active");
    toast(`${button.textContent?.slice(0, 2)} selected · generation snapshot loaded`);
  }));
  $("#refreshContext").addEventListener("click", () => toast("Context rebuild requested; pinned constraints retained"));
  $("#showDropped").addEventListener("click", () => toast("14 dropped items · mostly ephemeral grep output and raw history"));
  $("#collapseAgents").addEventListener("click", () => {
    const rows = $$(".agent-row.child, .agent-metrics, .agent-profile", $("#agentTree"));
    const hidden = rows.some(row => !(row as HTMLElement).hidden);
    rows.forEach(row => ((row as HTMLElement).hidden = hidden));
    $("#collapseAgents").textContent = hidden ? "Expand all" : "Collapse all";
  });
  $("#modeButton").addEventListener("click", () => {
    const current = $("#modeButton").textContent?.split(" ")[0] ?? "Build";
    const modes = ["Plan", "Build", "Review"];
    const next = modes[(modes.indexOf(current) + 1) % modes.length];
    selectMode(next);
    toast(`${next} mode selected`);
  });
  $("#attachButton").addEventListener("click", () => toast("Attach file · choose workspace file"));
  const input = $("#promptInput") as HTMLTextAreaElement;
  input.addEventListener("input", () => $("#mentionMenu").classList.toggle("open", input.value.includes("@")));
  $$("[data-mention]").forEach(button => button.addEventListener("click", () => {
    const mention = (button as HTMLElement).dataset.mention ?? "";
    input.value = `${input.value.replace(/@[^\s]*$/, "")}@${mention} `;
    $("#mentionMenu").classList.remove("open");
    input.focus();
  }));
  $("#composerForm").addEventListener("submit", event => {
    event.preventDefault();
    const value = input.value.trim();
    if (!value) { toast("Enter task for Primary"); input.focus(); return; }
    const message = el("article", "message user");
    const wrap = el("div");
    wrap.append(el("div", "message-meta", "You · now"), el("div", "bubble", value));
    message.append(wrap);
    $("#demoMessages").append(message);
    input.value = "";
    $("#threadScroll").scrollTop = $("#threadScroll").scrollHeight;
    toast("Task sent to Primary");
  });
  $("#paletteButton").addEventListener("click", openPalette);
  $("#paletteBackdrop").addEventListener("click", event => { if (event.target === $("#paletteBackdrop")) closePalette(); });
  $("#paletteInput").addEventListener("input", event => drawCommands((event.currentTarget as HTMLInputElement).value));
  $("#paletteInput").addEventListener("keydown", event => {
    if ((event as KeyboardEvent).key !== "Enter") return;
    const first = $("#paletteResults").querySelector<HTMLButtonElement>("button[role='option']");
    if (first) { event.preventDefault(); first.click(); }
  });
  document.addEventListener("keydown", event => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") { event.preventDefault(); openPalette(); }
    if (event.key === "Escape" && $("#paletteBackdrop").classList.contains("open")) closePalette();
  });
  $$(".code-head button").forEach(button => button.addEventListener("click", () => {
    const code = button.closest(".codeblock")?.querySelector("pre")?.textContent ?? "";
    void navigator.clipboard?.writeText(code).then(() => toast("Code copied"), () => toast("Clipboard unavailable in this browser context"));
  }));
}

init();
