package workspace

import (
	"strconv"
	"strings"
	"time"
)

// snapshotSubject is the subject of the commits snapshotCommit records of the user's working tree. A base with any
// other subject carries proposals nobody accepted (an agent's stacked dependencies), which are not the user's files.
const snapshotSubject = "relo: working tree snapshot"

// maxRecoverScan bounds how many recorded bases RecoverableCopy looks through.
const maxRecoverScan = 300

// RecoveredCopy is a file Relo recorded earlier: a snapshot commit of the user's working tree under refs/relo whose
// tree holds the path.
type RecoveredCopy struct {
	Ref    string    `json:"ref"`
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
}

// RecoverableCopy finds the newest copy of rel in the working-tree snapshots Relo cut agents from. It is how a file
// that has vanished from the working tree is brought back: `git show <commit>:<rel>`. Only snapshots of the user's
// own tree count, never an agent's edits or the proposals stacked under a dependent.
func (m *Manager) RecoverableCopy(rel string) (RecoveredCopy, bool) {
	if !isGit(m.root) || validateRelative(rel) != nil {
		return RecoveredCopy{}, false
	}
	out, err := gitRaw(m.root, "for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%09%(objectname)%09%(creatordate:unix)%09%(contents:subject)", "refs/relo/vws/")
	if err != nil {
		return RecoveredCopy{}, false
	}
	scanned := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 4 || !strings.HasSuffix(f[0], "/base") || f[3] != snapshotSubject {
			continue
		}
		if scanned++; scanned > maxRecoverScan {
			break
		}
		if _, err := git(m.root, "cat-file", "-e", f[1]+":"+rel); err != nil {
			continue
		}
		at, _ := strconv.ParseInt(f[2], 10, 64)
		return RecoveredCopy{Ref: f[0], Commit: f[1], At: time.Unix(at, 0).UTC()}, true
	}
	return RecoveredCopy{}, false
}
