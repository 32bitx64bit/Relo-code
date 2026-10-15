package ctxeng

import (
	"fmt"
	"strings"
)

func CompressItem(it Item) Item {
	body := compressText(it)
	out := it
	out.Content = body
	out.Source = SourceSummary
	out.Class = it.Class
	if out.Class == ClassHistory || out.Class == ClassEphemeral {
		out.Class = ClassTask
	}
	out.Tokens = Tokens(body)
	out.Durability = max(it.Durability, 0.8)
	return out
}

func compressText(it Item) string {
	var b strings.Builder
	title := it.Title
	if title == "" {
		title = firstLine(it.Content)
	}
	if it.Kind != "" {
		fmt.Fprintf(&b, "%s: %s\n", displayKind(it.Kind), title)
	} else {
		fmt.Fprintf(&b, "%s\n", title)
	}
	if loc := locations(it); loc != "" {
		fmt.Fprintf(&b, "Location: %s\n", loc)
	}
	if block := section(it.Content, "root cause"); block != "" {
		fmt.Fprintf(&b, "\nRoot cause:\n%s\n", block)
	}
	if block := section(it.Content, "implementation"); block != "" {
		fmt.Fprintf(&b, "\nImplementation:\n%s\n", block)
	}
	if block := section(it.Content, "verification"); block != "" {
		fmt.Fprintf(&b, "\nVerification:\n%s\n", block)
	}
	if block := section(it.Content, "open risk"); block != "" {
		fmt.Fprintf(&b, "\nOpen risk:\n%s\n", block)
	}
	if it.Verified {
		b.WriteString("\nVerification state: verified\n")
	} else if it.Source == SourceHypothesis {
		b.WriteString("\nVerification state: unverified hypothesis\n")
	}
	if b.Len() < 80 {
		b.WriteString("\n")
		b.WriteString(trimRunes(it.Content, 800))
	}
	return strings.TrimSpace(b.String())
}

func CompressReport(summary string, files []string, verify []string, risks []string, cause string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(summary))
	b.WriteString("\n")
	if cause != "" {
		fmt.Fprintf(&b, "\nRoot cause:\n%s\n", strings.TrimSpace(cause))
	}
	if len(files) > 0 {
		fmt.Fprintf(&b, "\nImplementation:\n%s\n", strings.Join(files, "\n"))
	}
	if len(verify) > 0 {
		fmt.Fprintf(&b, "\nVerification:\n%s\n", strings.Join(verify, "\n"))
	}
	if len(risks) > 0 {
		fmt.Fprintf(&b, "\nOpen risk:\n%s\n", strings.Join(risks, "\n"))
	} else {
		b.WriteString("\nOpen risk:\nnone reported\n")
	}
	return strings.TrimSpace(b.String())
}

func ParentSummary(it Item) Item {
	out := CompressItem(it)
	out.Class = ClassReport
	out.Source = SourceParent
	out.Durability = 0.9
	return out
}

func displayKind(k string) string {
	switch k {
	case KindArchitecture:
		return "Architecture"
	case KindBug:
		return "Bug"
	case KindDependency:
		return "Dependency"
	case KindConstraint:
		return "Constraint"
	case KindTest:
		return "Test"
	case KindPerformance:
		return "Performance"
	case KindSecurity:
		return "Security"
	case KindAPI:
		return "API"
	case KindSymbol:
		return "Symbol"
	default:
		if k == "" {
			return "Note"
		}
		return strings.ToUpper(k[:1]) + k[1:]
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return trimRunes(s, 160)
}

func locations(it Item) string {
	parts := append([]string{}, it.Files...)
	parts = append(parts, it.Symbols...)
	return strings.Join(parts, ", ")
}

// asciiLower lowers only A-Z. Unlike strings.ToLower it preserves byte length, so
// offsets found in the lowered copy are valid offsets into the original string.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func section(s, name string) string {
	idx := strings.Index(asciiLower(s), name)
	if idx < 0 {
		return ""
	}
	rest := s[idx:]
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	}
	cut := len(rest)
	lowerRest := asciiLower(rest)
	for _, stop := range []string{"\nroot cause", "\nimplementation", "\nverification", "\nopen risk"} {
		if p := strings.Index(lowerRest, stop); p > 0 && p < cut {
			cut = p
		}
	}
	return strings.TrimSpace(trimRunes(rest[:cut], 600))
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
