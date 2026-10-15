package id

import (
	"errors"
	"strings"
	"testing"
)

func TestNewFallsBackWhenRandomUnavailable(t *testing.T) {
	read := func([]byte) (int, error) { return 0, errors.New("random unavailable") }
	a, b := newWithRandom("test", read), newWithRandom("test", read)
	if !strings.HasPrefix(a, "test_") || a == b {
		t.Fatalf("fallback IDs not unique: %q %q", a, b)
	}
}
