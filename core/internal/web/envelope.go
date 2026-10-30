package web

import "strings"

const (
	untrustedOpen  = "<untrusted-content>\n"
	untrustedClose = "\n</untrusted-content>"
)

// untrustedEnvelope makes the trust boundary explicit in the Result itself,
// rather than relying on every transport to remember a label.
func untrustedEnvelope(content string) string {
	return untrustedOpen + content + untrustedClose
}

func appendNote(notes []string, note string) []string {
	if strings.TrimSpace(note) == "" {
		return notes
	}
	for _, existing := range notes {
		if existing == note {
			return notes
		}
	}
	return append(notes, note)
}
