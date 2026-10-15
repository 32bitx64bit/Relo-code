package orchestrator

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

type replyCase struct {
	name string
	body string
	want string
	done bool
}

var replyCases = []replyCase{
	{
		name: "simple",
		body: `{"status":"working","userMessage":"Hello world","tasks":[]}`,
		want: "Hello world", done: true,
	},
	{
		name: "escapes",
		body: `{"userMessage":"Hello \"x\"\nline2\t\\ \/ \b\f\r"}`,
		want: "Hello \"x\"\nline2\t\\ / \b\f\r", done: true,
	},
	{
		name: "unicode raw utf8",
		body: `{"userMessage":"Xin chào 世界 ✓"}`,
		want: "Xin chào 世界 ✓", done: true,
	},
	{
		name: "emoji raw utf8",
		body: `{"userMessage":"smile 😀 end"}`,
		want: "smile 😀 end", done: true,
	},
	{
		name: "unicode escapes and surrogate pair",
		body: `{"userMessage":"A\u00e9\u4e16 \ud83d\ude00 z"}`,
		want: "Aé世 😀 z", done: true,
	},
	{
		name: "lone surrogate becomes replacement",
		body: `{"userMessage":"a\ud83db\ude00c"}`,
		want: "a\uFFFDb\uFFFDc", done: true,
	},
	{
		name: "code fenced",
		body: "```json\n{\"status\":\"done\",\"userMessage\":\"fenced\"}\n```",
		want: "fenced", done: true,
	},
	{
		name: "bare fence with leading whitespace",
		body: "  \n```\n{\"userMessage\":\"bare\"}\n```\n",
		want: "bare", done: true,
	},
	{
		name: "leading prose",
		body: "Sure, here is the turn:\n{\"userMessage\":\"after prose\"}",
		want: "after prose", done: true,
	},
	{
		name: "userMessage not first key",
		body: `{"tasks":[],"status":"working","userMessage":"late key"}`,
		want: "late key", done: true,
	},
	{
		name: "nested userMessage in tasks array ignored",
		body: `{"tasks":[{"userMessage":"nested","description":"x"}],"userMessage":"top"}`,
		want: "top", done: true,
	},
	{
		name: "nested object userMessage ignored",
		body: `{"meta":{"userMessage":"inner"},"userMessage":"outer"}`,
		want: "outer", done: true,
	},
	{
		name: "userMessage text inside string value ignored",
		body: `{"tasks":[{"description":"set \"userMessage\": \"fake\" here"}],"note":"\"userMessage\":\"no\"","userMessage":"real"}`,
		want: "real", done: true,
	},
	{
		name: "userMessage null",
		body: `{"userMessage":null,"tasks":[]}`,
		want: "", done: true,
	},
	{
		name: "userMessage number",
		body: `{"userMessage":42}`,
		want: "", done: true,
	},
	{
		name: "userMessage object",
		body: `{"userMessage":{"text":"x"}}`,
		want: "", done: true,
	},
	{
		name: "empty string",
		body: `{"userMessage":""}`,
		want: "", done: true,
	},
	{
		name: "very long message",
		body: `{"userMessage":"` + strings.Repeat("abc \\\"q\\\" 😀 é\\n ", 150) + `"}`,
		want: strings.Repeat("abc \"q\" 😀 é\n ", 150), done: true,
	},
	{
		name: "missing key",
		body: `{"status":"working","tasks":[{"description":"no message"}]}`,
		want: "", done: false,
	},
	{
		name: "unterminated value not done",
		body: `{"userMessage":"partial`,
		want: "partial", done: false,
	},
}

// feedChecked feeds chunks, asserting monotonic cumulative text and that
// changed matches whether the text grew.
func feedChecked(t *testing.T, chunks []string) *replyStream {
	t.Helper()
	var r replyStream
	prev := ""
	for _, c := range chunks {
		text, changed := r.Feed(c)
		if !strings.HasPrefix(text, prev) {
			t.Fatalf("not monotonic: %q is not prefix-extension of %q", text, prev)
		}
		if changed != (text != prev) {
			t.Fatalf("changed=%v but text %q -> %q", changed, prev, text)
		}
		if text != r.Text() {
			t.Fatalf("Feed text %q != Text() %q", text, r.Text())
		}
		prev = text
	}
	return &r
}

func checkCase(t *testing.T, tc replyCase, r *replyStream) {
	t.Helper()
	if got := r.Text(); got != tc.want {
		t.Errorf("Text() = %q, want %q", got, tc.want)
	}
	if r.Done() != tc.done {
		t.Errorf("Done() = %v, want %v", r.Done(), tc.done)
	}
}

func TestReplyStreamTable(t *testing.T) {
	for _, tc := range replyCases {
		t.Run(tc.name, func(t *testing.T) {
			// Cross-check expectation against encoding/json when body is pure JSON.
			if trimmed := strings.TrimSpace(tc.body); strings.HasPrefix(trimmed, "{") {
				var v struct {
					UserMessage *string `json:"userMessage"`
				}
				if err := json.Unmarshal([]byte(trimmed), &v); err == nil && tc.done && v.UserMessage != nil {
					if *v.UserMessage != tc.want {
						t.Fatalf("test expectation %q disagrees with json %q", tc.want, *v.UserMessage)
					}
				}
			}

			// (a) all at once
			checkCase(t, tc, feedChecked(t, []string{tc.body}))

			// (b) one byte at a time (splits every multi-byte rune and escape)
			bytes := make([]string, 0, len(tc.body))
			for i := 0; i < len(tc.body); i++ {
				bytes = append(bytes, tc.body[i:i+1])
			}
			checkCase(t, tc, feedChecked(t, bytes))

			// (c) every single split point
			for i := 0; i <= len(tc.body); i++ {
				checkCase(t, tc, feedChecked(t, []string{tc.body[:i], tc.body[i:]}))
			}

			// (d) random split sizes, fixed seed
			rng := rand.New(rand.NewSource(1))
			for iter := 0; iter < 50; iter++ {
				var chunks []string
				for pos := 0; pos < len(tc.body); {
					n := 1 + rng.Intn(8)
					if pos+n > len(tc.body) {
						n = len(tc.body) - pos
					}
					chunks = append(chunks, tc.body[pos:pos+n])
					pos += n
				}
				checkCase(t, tc, feedChecked(t, chunks))
			}
		})
	}
}

func TestReplyStreamMatchesJSONUnmarshal(t *testing.T) {
	body := `{"status":"working","userMessage":"Hello \"x\"\nline2 😀 \u00e9 \ud83d\ude00","tasks":[{"userMessage":"no"}]}`
	var v struct {
		UserMessage string `json:"userMessage"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	r := feedChecked(t, []string{body})
	if r.Text() != v.UserMessage {
		t.Fatalf("Text() = %q, json = %q", r.Text(), v.UserMessage)
	}
}

func TestReplyStreamResetAndNoEmitAfterDone(t *testing.T) {
	var r replyStream
	r.Feed(`{"userMessage":"one"`)
	if _, changed := r.Feed(`}, "userMessage":"two"`); changed {
		t.Fatal("emitted after Done")
	}
	if r.Text() != "one" || !r.Done() {
		t.Fatalf("Text() = %q, Done() = %v", r.Text(), r.Done())
	}
	r.Reset()
	if r.Text() != "" || r.Done() {
		t.Fatal("Reset did not clear state")
	}
	r.Feed(`{"userMessage":"again"}`)
	if r.Text() != "again" {
		t.Fatalf("after Reset Text() = %q", r.Text())
	}
}
