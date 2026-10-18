package workspace

// SetSnapshotFault is a test seam for packages outside workspace. Mode "fallback" makes Snapshot record HEAD with
// HeadFallback set, and "fail" makes it fail closed as an unverifiable snapshot does. Any other mode restores the
// real snapshot.
func (m *Manager) SetSnapshotFault(mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch mode {
	case "fallback":
		m.snapshotCommitFn = func(s Snapshot) (string, bool) { return s.Head, true }
	case "fail":
		m.snapshotCommitFn = func(Snapshot) (string, bool) { return "", false }
	default:
		m.snapshotCommitFn = nil
	}
}
