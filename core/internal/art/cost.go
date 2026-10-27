package art

import "strings"

// Prices are in US dollars. OpenAI bills the image models by token, and says to read the usage in the response for the
// real figure; the table below only turns that usage into dollars, and gives an estimate before the call that is meant
// to be high, not exact. Both come from OpenAI's published prices (https://developers.openai.com/api/docs/pricing) and
// are worth re-checking when the models change.

// tokenRates are dollars per million tokens.
type tokenRates struct{ TextIn, ImageIn, ImageOut float64 }

var modelRates = map[string]tokenRates{
	"gpt-image-2.5-sunburst": {5, 8, 30},
	"gpt-image-2.5-flare":    {5, 8, 30},
	"gpt-image-2":            {5, 8, 30},
	"gpt-image-1.5":          {5, 8, 32},
	"chatgpt-image-latest":   {5, 8, 32},
	"gpt-image-1-mini":       {2, 2.5, 8},
}

// unknownRates are the highest known rates, so a model this table has not heard of is never cheaper than it looks.
var unknownRates = tokenRates{5, 8, 32}

func ratesFor(model string) tokenRates {
	if r, ok := modelRates[strings.ToLower(strings.TrimSpace(model))]; ok {
		return r
	}
	return unknownRates
}

// Usage is the token count an image response reports.
type Usage struct {
	TextIn   int `json:"textIn"`
	ImageIn  int `json:"imageIn"`
	ImageOut int `json:"imageOut"`
}

// CostUSD turns reported usage into dollars.
func CostUSD(model string, u Usage) float64 {
	r := ratesFor(model)
	return (float64(u.TextIn)*r.TextIn + float64(u.ImageIn)*r.ImageIn + float64(u.ImageOut)*r.ImageOut) / 1e6
}

// perImageUSD is the price of one picture before references, by quality and shape. The figures are OpenAI's listing for
// gpt-image-2, the highest of the models this table knows. "auto" is priced as medium.
var perImageUSD = map[string][2]float64{ // quality -> {square, rectangle}
	"low":    {0.006, 0.005},
	"medium": {0.053, 0.041},
	"high":   {0.211, 0.165},
}

// refInputTokens is what one reference picture is assumed to cost as input, rounded up.
const refInputTokens = 1500

// EstimateUSD is what a job should cost at most: the pictures asked for, plus each reference as image input and the
// prompt as text.
func EstimateUSD(model, quality, size string, count, refs, promptBytes int) float64 {
	price, ok := perImageUSD[strings.ToLower(quality)]
	if !ok {
		price = perImageUSD["medium"]
	}
	each := price[0]
	if w, h, ok := parseSize(size); ok && w != h {
		each = price[1]
	}
	r := ratesFor(model)
	in := float64(refs*refInputTokens)*r.ImageIn + float64(promptBytes/3+1)*r.TextIn
	return round4(float64(max(1, count))*each + in/1e6)
}

func round4(v float64) float64 { return float64(int(v*10000+0.5)) / 10000 }

// parseSize reads WIDTHxHEIGHT.
func parseSize(s string) (w, h int, ok bool) {
	a, b, found := strings.Cut(strings.ToLower(s), "x")
	if !found {
		return 0, 0, false
	}
	for _, p := range []struct {
		src string
		dst *int
	}{{a, &w}, {b, &h}} {
		n := 0
		if p.src == "" {
			return 0, 0, false
		}
		for _, r := range p.src {
			if r < '0' || r > '9' {
				return 0, 0, false
			}
			n = n*10 + int(r-'0')
		}
		*p.dst = n
	}
	return w, h, w > 0 && h > 0
}
