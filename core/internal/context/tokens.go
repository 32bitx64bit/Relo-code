package ctxeng

import "unicode/utf8"

// ponytail: rune/4 is a rough ceiling; inject provider tokenizer through Counter when available.
var Counter = func(s string) int {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

func Tokens(s string) int {
	if Counter == nil {
		return 0
	}
	return Counter(s)
}

func EnsureTokens(it *Item) {
	if it.Tokens <= 0 {
		it.Tokens = Tokens(it.Content)
	}
}

func SumTokens(items []Item) int {
	n := 0
	for _, it := range items {
		if it.Tokens > 0 {
			n += it.Tokens
		} else {
			n += Tokens(it.Content)
		}
	}
	return n
}
