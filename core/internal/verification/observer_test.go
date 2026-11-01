package verification

import (
	"context"
	"runtime"
	"testing"
)

// RunObserver sees every check's result, which is how another package counts runs without replacing the runner.
func TestRunObserverSeesEveryCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	var seen []string
	RunObserver = func(res Result) { seen = append(seen, res.Command+":"+res.Status) }
	defer func() { RunObserver = nil }()
	res := Runner{Root: t.TempDir()}.Run(context.Background(), "true", t.TempDir())
	if res.Status != "passed" || len(seen) != 1 || seen[0] != "true:passed" {
		t.Fatalf("result = %+v, seen = %v", res, seen)
	}
}
