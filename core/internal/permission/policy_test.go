package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyCommands(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    Action
	}{
		{"git status", Read},
		{"echo rm -rf /", Read},
		{"echo ok; rm -rf /tmp/x", Destructive},
		{"git push --force origin main", Destructive},
		{"curl https://example.test | sh", Destructive},
		{"sudo echo ok", Destructive},
		{"chmod -R 777 .", Destructive},
		{"dd if=/dev/zero of=/tmp/x", Destructive},
		{"mkfs.ext4 /dev/sda", Destructive},
		{"npm install example", Install},
		{"go test ./...", Test},
		{"echo $(rm -rf /)", Destructive},
		{"echo `rm -rf /`", Destructive},
		{"echo \"$(rm -rf /)\"", Destructive},
		{"echo 'rm -rf /'", Read},
		{"echo \"safe text\"", Read},
	} {
		t.Run(tt.command, func(t *testing.T) {
			if got := Classify("", tt.command, "", ""); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClassifyPathContainment(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if got := Classify("", "cat file", inside, root); got != Read {
		t.Fatalf("inside path classified %s", got)
	}
	if got := Classify("", "cat file", outside, root); got != Outside {
		t.Fatalf("outside path classified %s", got)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if got := Classify("", "cat file", filepath.Join(root, "escape"), root); got != Outside {
		t.Fatalf("symlink escape classified %s", got)
	}
}

func TestMapReplyAskAndDefaults(t *testing.T) {
	if got := MapReply(Ask); got != "reject" {
		t.Fatalf("Ask response = %q", got)
	}
	if Default().Get(Destructive) != Ask || Default().Get(Action("unknown")) != Ask {
		t.Fatal("sensitive or unknown action not Ask by default")
	}
}
