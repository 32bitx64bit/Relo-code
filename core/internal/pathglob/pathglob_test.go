package pathglob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, rel string
		want         bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/deep/a.md", true},
		{"*.md", "docs/a.mdx", false},
		{"RELO_TASKS.md", "RELO_TASKS.md", true},
		{"RELO_TASKS.md", "sub/RELO_TASKS.md", true},
		{".relo/**", ".relo/relo.db", true},
		{".relo/**", ".relo/worktrees/a/x", true},
		{".relo/**", "src/.relo/x", false},
		{"**/obj/**", "obj/x.dll", true},
		{"**/obj/**", "Mod/Source/obj/Release/x.dll", true},
		{"**/obj/**", "Mod/object/x.dll", false},
		{"**/bin/Debug/**", "bench/t/bin/Debug/net10.0/a.dll", true},
		{"**/bin/Debug/**", "bench/t/bin/Release/a.dll", false},
		{"Mod/Assemblies/*.dll", "Mod/Assemblies/Mod.dll", true},
		{"Mod/Assemblies/*.dll", "Other/Mod/Assemblies/Mod.dll", false},
		{"Mod/Assemblies/*.dll", "Mod/Assemblies/sub/Mod.dll", false},
		{"**", "anything/at/all", true},
		{"", "a", false},
		{"*.md", "", false},
		{"[", "a", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.rel); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}

func TestAny(t *testing.T) {
	if !Any([]string{"*.txt", "**/obj/**"}, "a/obj/x") {
		t.Fatal("second pattern should match")
	}
	if Any(nil, "a") || Any([]string{"*.txt"}, "a.md") {
		t.Fatal("no pattern should match")
	}
}
