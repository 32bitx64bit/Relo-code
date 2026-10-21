package vws

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reference describes changes; it does not deliver them. The recipient sees none of it until it imports, and then
// only the paths it asks for.
func TestAReferenceIsVisibleOnlyThroughAnExplicitSelectiveImport(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	sender := newWS(t, e, "sender")
	recipient := newWS(t, e, "recipient")
	mustWrite(t, sender, "api.go", "package api\n\nfunc New() {}\n")
	mustWrite(t, sender, "docs/api.md", "the api\n")
	mustWrite(t, sender, "README.md", "sender readme\n")

	ref, err := sender.Reference([]string{"docs/api.md", "api.go", "api.go"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.SenderAgent != "sender" || ref.Branch != "relo/sender" || ref.Commit == "" || ref.BaseCommit != sender.Info().BaseCommit {
		t.Fatalf("reference = %+v", ref)
	}
	if strings.Join(ref.Paths, ",") != "api.go,docs/api.md" {
		t.Fatalf("paths = %v (sorted, deduplicated, and only what was named)", ref.Paths)
	}
	mustNotExist(t, recipient, "api.go") // holding a reference grants no visibility
	mustNotExist(t, recipient, "docs/api.md")

	// The sender keeps working; the reference still means what it meant when it was sent.
	mustWrite(t, sender, "api.go", "package api\n\nfunc Newer() {}\n")

	before := recipient.Info().Revision
	res, names, err := e.ImportReference("recipient", ref, []string{"api.go"}, ApplyOptions{})
	if err != nil || !res.Changed || strings.Join(names, ",") != "api.go" {
		t.Fatalf("import = %+v %v %v", res, names, err)
	}
	if got := mustRead(t, recipient, "api.go"); got != "package api\n\nfunc New() {}\n" {
		t.Fatalf("imported the sender's later edit instead of the referenced revision: %q", got)
	}
	mustNotExist(t, recipient, "docs/api.md") // not asked for, not delivered
	if got := mustRead(t, recipient, "README.md"); got != "hello\n" {
		t.Fatalf("a file the reference was never asked for changed: %q", got)
	}
	if recipient.Info().Revision <= before {
		t.Fatal("the import did not publish a revision")
	}
	if got := recipient.Info().Meta["import:sender"]; got == "" {
		t.Fatalf("the import is not recorded in the workspace's metadata: %+v", recipient.Info().Meta)
	}

	// Asking for a path the reference does not name is refused, whatever the sender's workspace holds.
	if _, _, err := e.ImportReference("recipient", ref, []string{"README.md"}, ApplyOptions{}); err == nil || !strings.Contains(err.Error(), "not named by the reference") {
		t.Fatalf("import of an unnamed path: %v", err)
	}
	// With no list, everything the reference names comes.
	if _, names, err := e.ImportReference("recipient", ref, nil, ApplyOptions{}); err != nil || len(names) != 2 {
		t.Fatalf("whole import = %v %v", names, err)
	}
	if got := mustRead(t, recipient, "docs/api.md"); got != "the api\n" {
		t.Fatalf("docs/api.md = %q", got)
	}
}

func TestAReferenceNamesOnlyWhatTheSenderChanged(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	sender := newWS(t, e, "sender")
	mustWrite(t, sender, "api.go", "package api\n")
	for _, bad := range [][]string{nil, {}, {"README.md"}, {".git/config"}, {"../x"}, {"/etc/passwd"}, {"api.go", "nothing.go"}} {
		if _, err := sender.Reference(bad); err == nil {
			t.Fatalf("a reference to %v was accepted", bad)
		}
	}
	many := make([]string, MaxReferencePaths+1)
	for i := range many {
		many[i] = "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".txt"
		mustWrite(t, sender, many[i], "x\n")
	}
	if _, err := sender.Reference(many); err == nil {
		t.Fatal("a reference may not name more than MaxReferencePaths files")
	}
	// A deletion is a change, and importing it deletes the file for the recipient too.
	if err := sender.DeleteFile("README.md"); err != nil {
		t.Fatal(err)
	}
	ref, err := sender.Reference([]string{"README.md"})
	if err != nil {
		t.Fatalf("a deletion cannot be referenced: %v", err)
	}
	recipient := newWS(t, e, "recipient")
	if _, _, err := e.ImportReference("recipient", ref, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, recipient, "README.md")
}

func TestAReferenceCannotBeForgedOrOutliveItsSender(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	sender := newWS(t, e, "sender")
	other := newWS(t, e, "other")
	recipient := newWS(t, e, "recipient")
	mustWrite(t, sender, "api.go", "package api\n")
	mustWrite(t, other, "secret.go", "package secret\n")
	ref, err := sender.Reference([]string{"api.go"})
	if err != nil {
		t.Fatal(err)
	}
	otherRef, _ := other.Reference([]string{"secret.go"})

	forged := ref
	forged.Commit = otherRef.Commit // a real commit, but not the one the sender pinned
	if _, _, err := e.ImportReference("recipient", forged, nil, ApplyOptions{}); !errors.Is(err, ErrNoReference) {
		t.Fatalf("forged commit: %v", err)
	}
	forged = ref
	forged.SenderAgent = "other" // someone else's name on the sender's commit
	forged.Paths = []string{"api.go"}
	if _, _, err := e.ImportReference("recipient", forged, nil, ApplyOptions{}); !errors.Is(err, ErrNoReference) {
		t.Fatalf("forged sender: %v", err)
	}
	forged = ref
	forged.Revision += 5
	if _, _, err := e.ImportReference("recipient", forged, nil, ApplyOptions{}); !errors.Is(err, ErrNoReference) {
		t.Fatalf("forged revision: %v", err)
	}
	if _, _, err := e.ImportReference("sender", ref, nil, ApplyOptions{}); err == nil {
		t.Fatal("an agent imported its own change")
	}
	mustNotExist(t, recipient, "api.go")
	mustNotExist(t, recipient, "secret.go")

	// Once the sender's workspace is gone the reference is too.
	if err := sender.Dispose(false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ImportReference("recipient", ref, nil, ApplyOptions{}); !errors.Is(err, ErrNoReference) {
		t.Fatalf("after dispose: %v", err)
	}
}

func TestImportIsRefusedForReadOnlyRecipientsAndStaleWriters(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	sender := newWS(t, e, "sender")
	mustWrite(t, sender, "api.go", "package api\n")
	ref, _ := sender.Reference([]string{"api.go"})
	reader, err := e.Create("reader", "HEAD", CreateOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ImportReference("reader", ref, nil, ApplyOptions{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only import: %v", err)
	}
	mustNotExist(t, reader, "api.go")
	recipient := newWS(t, e, "recipient")
	old := recipient.Info().Revision
	mustWrite(t, recipient, "mine.go", "package mine\n")
	if _, _, err := e.ImportReference("recipient", ref, nil, Expect(old)); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("an import decided on an old revision: %v", err)
	}
	mustNotExist(t, recipient, "api.go")
}

// While the recipient's tools run, its directory is the authority: the import is written there and captured, so it
// is not lost to the next capture and never races the tools.
func TestImportIntoARunningDirectoryIsCapturedLikeToolOutput(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	sender := newWS(t, e, "sender")
	recipient := newWS(t, e, "recipient")
	mustWrite(t, sender, "api.go", "package api\n")
	mustWrite(t, sender, "bin/run.sh", "#!/bin/sh\n")
	if err := sender.Chmod("bin/run.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sender.Symlink("api.link", "api.go"); err != nil {
		t.Fatal(err)
	}
	ref, err := sender.Reference([]string{"api.go", "bin/run.sh", "api.link"})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := recipient.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ImportReference("recipient", ref, nil, ApplyOptions{}); !errors.Is(err, ErrMaterialized) {
		t.Fatalf("the overlay was edited under a running directory: %v", err)
	}
	names, err := e.ImportReferenceIntoExec(context.Background(), "recipient", ref, []string{"api.go", "bin/run.sh", "api.link"})
	if err != nil || len(names) != 3 {
		t.Fatalf("import into the directory = %v %v", names, err)
	}
	if b, _ := os.ReadFile(filepath.Join(ex.Dir, "api.go")); string(b) != "package api\n" {
		t.Fatalf("api.go in the directory = %q", b)
	}
	if st, err := os.Stat(filepath.Join(ex.Dir, "bin", "run.sh")); err != nil || st.Mode()&0o100 == 0 {
		t.Fatalf("executable bit lost: %v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(ex.Dir, "api.link")); err != nil || target != "api.go" {
		t.Fatalf("link = %q %v", target, err)
	}
	if got := mustRead(t, recipient, "api.go"); got != "package api\n" {
		t.Fatalf("the overlay was not updated by the capture: %q", got)
	}
}

func TestImportIntoADirectoryNeverFollowsALinkPlantedByATool(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	sender := newWS(t, e, "sender")
	recipient := newWS(t, e, "recipient")
	mustWrite(t, sender, "sub/api.go", "package api\n")
	ref, _ := sender.Reference([]string{"sub/api.go"})
	ex, err := recipient.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ex.Dir, "sub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := e.ImportReferenceIntoExec(context.Background(), "recipient", ref, nil); !errors.Is(err, ErrSymlink) {
		t.Fatalf("import through a planted link: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("a file was written outside the workspace: %v", entries)
	}
}

func TestImportRefusesALinkThatLeavesTheWorkspace(t *testing.T) {
	dir := newRepo(t, baseFiles)
	e := openEngine(t, dir)
	sender := newWS(t, e, "sender")
	recipient := newWS(t, e, "recipient")
	if err := sender.Symlink("escape", "../../etc/passwd"); err == nil {
		// The sender's own overlay already refuses it; nothing to reference.
		t.Fatal("the overlay accepted a link out of the workspace")
	}
	mustWrite(t, sender, "ok.go", "package ok\n")
	ref, _ := sender.Reference([]string{"ok.go"})
	if _, _, err := e.ImportReference("recipient", ref, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, recipient, "ok.go"); got != "package ok\n" {
		t.Fatalf("ok.go = %q", got)
	}
}
