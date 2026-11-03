package runtime

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ErrorClass groups a runtime error by what a backup model can fix.
type ErrorClass string

const (
	ClassQuota            ErrorClass = "quota"
	ClassRateLimit        ErrorClass = "rate_limit"
	ClassContext          ErrorClass = "context"
	ClassAuth             ErrorClass = "auth"
	ClassModelUnavailable ErrorClass = "model_unavailable"
	ClassServer           ErrorClass = "server"
	ClassCrash            ErrorClass = "crash"
	ClassUnknown          ErrorClass = "unknown"
)

// Classify maps a runtime error to an ErrorClass. A nil error is unknown.
func Classify(err error) ErrorClass {
	if err == nil {
		return ClassUnknown
	}
	return ClassifyText(err.Error())
}

// ClassifyText classifies an error message. The message may be plain text or contain a JSON error object.
// Shapes handled:
//   - OpenCode session errors: plain message text only. client.go:949-953 copies data.error.message
//     (or .detail) into the event, so statusCode/responseBody never reach here (fixture: client_test.go:602).
//   - OpenCode, {"name":...,"data":{"message","statusCode","responseBody"}} (not in repo fixtures; hypothesis)
//   - Claude Code result line, {"is_error","subtype","api_error_status","result","errors"} (hypothesis for api_error_status; translate.go:262)
//
// Structured fields are read first; the text of every field is then matched against the phrase patterns.
// A 400 status is auth only when the text has OAuth-style auth wording; other 400s stay unknown.
func ClassifyText(msg string) ErrorClass {
	status, text := 0, strings.ToLower(msg)
	if i := strings.IndexByte(msg, '{'); i >= 0 {
		var obj map[string]any
		if json.Unmarshal([]byte(msg[i:]), &obj) == nil {
			var parts []string
			status = collectErrorParts(obj, &parts, 0)
			text = strings.ToLower(msg + "\n" + strings.Join(parts, "\n"))
		}
	}
	if status == 0 {
		status = statusInText(text)
	}
	switch {
	case quotaRe.MatchString(text):
		return ClassQuota
	case contextRe.MatchString(text):
		return ClassContext
	case modelRe.MatchString(text):
		return ClassModelUnavailable
	case (status == 400 || status == 0) && auth400Re.MatchString(text):
		return ClassAuth
	case status == 401 || status == 403 || authRe.MatchString(text):
		return ClassAuth
	case status == 402:
		return ClassQuota
	case status == 413:
		return ClassContext
	case status == 404 && strings.Contains(text, "model"):
		return ClassModelUnavailable
	case status == 429 || rateRe.MatchString(text):
		return ClassRateLimit
	case (status >= 500 && status < 600) || serverRe.MatchString(text):
		return ClassServer
	case crashRe.MatchString(text):
		return ClassCrash
	}
	return ClassUnknown
}

var (
	quotaRe   = regexp.MustCompile(`usage limit|hit your limit|quota|insufficient_quota|credit balance|billing|out of (tokens|credits)|ran out of (tokens|credits)|(tokens|credits) (exhausted|used up)|token (budget|allowance)`)
	contextRe = regexp.MustCompile(`context[ _]?(length|window|overflow)|contextoverflow|maximum context|prompt is too long|too many tokens|input is too long`)
	modelRe   = regexp.MustCompile(`model[ _]?(not found|not_found|unavailable|does not exist|is not available)|unknown model|no such model|model_not_found`)
	authRe    = regexp.MustCompile(`unauthori[sz]ed|invalid api[ _]key|invalid_api_key|authenticat|providerautherror|not logged in|/login|forbidden`)
	// auth400Re matches OAuth-style auth errors that providers return with 400.
	auth400Re = regexp.MustCompile(`invalid_grant|invalid_client|invalid_token|unauthorized_client|token (is )?(expired|revoked)`)
	rateRe    = regexp.MustCompile(`rate[ _-]?limit|too many requests|\b429\b`)
	serverRe  = regexp.MustCompile(`internal server error|overloaded|bad gateway|service unavailable|gateway timeout|server error|api_error|connection (refused|reset)|econn(refused|reset)|no such host|i/o timeout|tls handshake|unexpected eof|network is unreachable|socket hang up|fetch failed|upstream (connect|request) (error|timeout)|empty response`)
	crashRe   = regexp.MustCompile(`crash|panic|segmentation fault|signal: |killed|exited|exit status|broken pipe|unexpectedly`)
	statusRe  = regexp.MustCompile(`(?:status|http|code)\D{0,12}(\d{3})\b`)
)

// collectErrorParts appends the message-bearing text of an error object and returns the HTTP-style status it carries.
func collectErrorParts(obj map[string]any, parts *[]string, depth int) int {
	if depth > 4 {
		return 0
	}
	status := 0
	for _, key := range []string{"statusCode", "api_error_status", "status"} {
		if n := numberOf(obj[key]); status == 0 && n > 0 {
			status = n
		}
	}
	for _, key := range []string{"name", "message", "detail", "responseBody", "result", "subtype", "terminal_reason"} {
		if s, ok := obj[key].(string); ok {
			*parts = append(*parts, s)
		}
	}
	if errs, ok := obj["errors"].([]any); ok {
		for _, e := range errs {
			if s, ok := e.(string); ok {
				*parts = append(*parts, s)
			}
		}
	}
	for _, key := range []string{"error", "data"} {
		switch v := obj[key].(type) {
		case string:
			*parts = append(*parts, v)
		case map[string]any:
			if n := collectErrorParts(v, parts, depth+1); status == 0 {
				status = n
			}
		}
	}
	return status
}

func numberOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// statusInText reads a status code from plain text such as "status code 429" or "HTTP 503".
func statusInText(text string) int {
	if m := statusRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}
