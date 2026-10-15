package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactsNestedFieldsAndStrings(t *testing.T) {
	var out bytes.Buffer
	New(&out, Info).With(map[string]any{"nested": map[string]any{"password": "dont-log"}}).Error("Authorization: Bearer abc.def and sk-abcdefghijk password=hunter2 OPENCODE_SERVER_PASSWORD=secret", map[string]any{
		"details": []any{map[string]any{"Authorization": "Bearer nestedtoken"}, "password=embedded"},
	})
	text := out.String()
	for _, secret := range []string{"dont-log", "abc.def", "sk-abcdefghijk", "hunter2", "secret", "nestedtoken", "embedded"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret %q leaked: %s", secret, text)
		}
	}
}
