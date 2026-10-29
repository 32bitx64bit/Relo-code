package claudecode

import (
	"fmt"
	"reflect"
	"testing"
)

// A tool_use message carries the complete input; the loop detector reads it from Raw.
func TestToolUseEventCarriesCompleteInput(t *testing.T) {
	var tr translator
	want := map[string]any{"file_path": "a.go", "offset": float64(1)}
	for i := 0; i < 3; i++ {
		line := []byte(fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t%d","name":"Read","input":{"file_path":"a.go","offset":1}}]}}`, i))
		events, _ := tr.feed(line)
		if len(events) != 1 || events[0].ToolID == "" {
			t.Fatalf("events = %#v", events)
		}
		if got := events[0].Raw["input"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("call %d input = %#v, want %#v", i, got, want)
		}
	}
}
