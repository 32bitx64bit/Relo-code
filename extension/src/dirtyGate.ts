// Paths in these helpers are workspace-relative and slash-separated, the same form the core stores on a change.

export interface DocumentState {
  path: string;
  isDirty: boolean;
}

function clean(p: string): string {
  return p.replace(/\\/g, "/").replace(/^\.\//, "");
}

// dirtyPathsFor returns the proposed paths that have an unsaved editor buffer. It is the gate's decision without
// touching VS Code, so it can be tested with fake documents.
export function dirtyPathsFor(paths: string[], docs: DocumentState[]): string[] {
  const wanted = new Set(paths.filter(Boolean).map(clean));
  const dirty = new Set<string>();
  for (const doc of docs) {
    const p = clean(doc.path);
    if (doc.isDirty && wanted.has(p)) dirty.add(p);
  }
  return [...dirty];
}
