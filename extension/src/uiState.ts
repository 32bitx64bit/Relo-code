// Collapse state for settings sections ("g:<group>") and model-picker providers ("p:<provider>").
// Pure helpers: no vscode import, so they run under tsx --test. Stored in globalState, never in settings.

export const COLLAPSE_KEY_RE = /^[gp]:[A-Za-z0-9._@/-]{1,80}$/;
export const MAX_COLLAPSED = 200;

export type Collapsed = Record<string, true>;

// Keeps only well-formed keys with value true. Anything else (prototype keys, other types, overflow) is dropped.
export function cleanCollapsed(raw: unknown): Collapsed {
  const out: Collapsed = Object.create(null) as Collapsed;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return { ...out };
  let n = 0;
  for (const key of Object.keys(raw)) {
    if (n >= MAX_COLLAPSED) break;
    if (COLLAPSE_KEY_RE.test(key) && (raw as Record<string, unknown>)[key] === true) {
      out[key] = true;
      n++;
    }
  }
  return { ...out };
}

// Applies one webview change. Invalid input returns the state unchanged.
export function applyCollapse(current: Collapsed, key: unknown, collapsed: unknown): Collapsed {
  if (typeof key !== "string" || !COLLAPSE_KEY_RE.test(key) || typeof collapsed !== "boolean") return current;
  const next = cleanCollapsed(current);
  if (collapsed) {
    if (!(key in next) && Object.keys(next).length >= MAX_COLLAPSED) return current;
    next[key] = true;
  } else {
    delete next[key];
  }
  return next;
}
