package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relocode/relo/internal/vws"
)

func senderWith(t *testing.T, m *Manager, id string) {
	t.Helper()
	m.Configure(Config{Mode: ModeVirtual})
	tree, err := m.EnsureTree(id, false)
	if err != nil || !tree.Virtual {
		t.Fatalf("sender tree = %+v %v", tree, err)
	}
	dir, err := m.Materialize(id)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "api/api.go", "package api\n")
	write(t, dir, "api/notes.md", "notes\n")
	write(t, dir, "unrelated.txt", "mine\n")
}

// What one agent says it built reaches another only through an import the recipient asks for, in every combination
// of how the two hold their files.
func TestChangeReferencesAreImportedExplicitlyBetweenAnyTwoAgents(t *testing.T) {
	for _, recipientMode := range []struct {
		name    Mode
		running bool
	}{{ModeVirtual, false}, {ModeVirtual, true}, {ModeWorktree, false}} {
		name := string(recipientMode.name)
		if recipientMode.running {
			name += "-running"
		}
		t.Run(name, func(t *testing.T) {
			m, dir := virtualRepo(t, ModeVirtual)
			senderWith(t, m, "sender")
			ref, err := m.Reference("sender", []string{"api/api.go", "api/notes.md"})
			if err != nil {
				t.Fatal(err)
			}
			if ref.SenderAgent != "sender" || ref.Commit == "" || len(ref.Paths) != 2 {
				t.Fatalf("reference = %+v", ref)
			}

			m.Configure(Config{Mode: recipientMode.name})
			tree, err := m.EnsureTree("recipient", false)
			if err != nil || tree.Virtual != (recipientMode.name == ModeVirtual) {
				t.Fatalf("recipient tree = %+v %v", tree, err)
			}
			v, err := m.View("recipient")
			if err != nil {
				t.Fatal(err)
			}
			if recipientMode.running {
				if _, err := m.Materialize("recipient"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := v.ReadFile("api/api.go"); !IsNotExist(err) {
				t.Fatalf("holding a reference made the code visible: %v", err)
			}

			// The sender keeps going; the recipient gets the referenced revision.
			sdir, _ := m.Materialize("sender")
			write(t, sdir, "api/api.go", "package api // later\n")

			names, err := m.ImportChange("recipient", ref, []string{"api/api.go"})
			if err != nil || strings.Join(names, ",") != "api/api.go" {
				t.Fatalf("import = %v %v", names, err)
			}
			if got, err := v.ReadFile("api/api.go"); err != nil || string(got) != "package api\n" {
				t.Fatalf("api.go = %q %v", got, err)
			}
			if _, err := v.ReadFile("api/notes.md"); !IsNotExist(err) {
				t.Fatalf("an unrequested path arrived: %v", err)
			}
			if _, err := m.ImportChange("recipient", ref, []string{"unrelated.txt"}); err == nil {
				t.Fatal("a path outside the reference was imported")
			}
			changes, err := v.Changes()
			if err != nil || len(changes) != 1 || changes[0].Path != "api/api.go" {
				t.Fatalf("the import is the recipient's own change: %+v %v", changes, err)
			}
			// The user's folder was never touched.
			if _, err := os.Stat(filepath.Join(dir, "api")); !os.IsNotExist(err) {
				t.Fatalf("an import reached the user's folder: %v", err)
			}
		})
	}
}

func TestChangeReferenceRefusals(t *testing.T) {
	m, _ := virtualRepo(t, ModeVirtual)
	senderWith(t, m, "sender")
	if _, err := m.Reference("sender", []string{"nothing-changed.go"}); err == nil {
		t.Fatal("a reference to a file the sender did not change was made")
	}

	// A worktree has no pinned revision to point at.
	m.Configure(Config{Mode: ModeWorktree})
	if _, err := m.EnsureTree("wt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reference("wt", []string{"x"}); !errors.Is(err, ErrNoReferences) {
		t.Fatalf("worktree sender: %v", err)
	}

	ref, err := m.Reference("sender", []string{"api/api.go"})
	if err != nil {
		t.Fatal(err)
	}
	// An agent that reads the user's folder has nowhere to put an import, and it never writes there.
	if _, err := m.EnsureTree("reader", true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ImportChange("reader", ref, nil); !errors.Is(err, ErrNoView) {
		t.Fatalf("shared-folder recipient: %v", err)
	}
	if _, err := m.ImportChange("nobody", ref, nil); !errors.Is(err, ErrNoView) {
		t.Fatalf("unknown recipient: %v", err)
	}
	if _, err := m.ImportChange("sender", ref, nil); err == nil {
		t.Fatal("a sender imported its own change")
	}

	// Releasing the sender ends its references.
	if err := m.Release("sender"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ImportChange("wt", ref, nil); !errors.Is(err, vws.ErrNoReference) {
		t.Fatalf("after the sender was released: %v", err)
	}
}
