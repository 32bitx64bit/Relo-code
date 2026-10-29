package runtime

import (
	"errors"
	"testing"
)

func TestClassifyTable(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want ErrorClass
	}{
		// OpenCode session.error / session.execution.failed. client.go:949-953 reads data.error.message
		// (then .detail) into Event.Error as plain text, so that text is all ClassifyText sees.
		// Fixture from opencode/client_test.go:602 carries message "boom".
		{"opencode plain boom", "boom", ClassUnknown},
		{"opencode message quota", "You exceeded your current quota", ClassQuota},
		{"opencode message rate", "rate limit exceeded", ClassRateLimit},
		{"opencode message auth", "invalid api key", ClassAuth},
		{"opencode message 401", "401 Unauthorized", ClassAuth},
		{"opencode message 403", "403 Forbidden", ClassAuth},
		{"opencode message context", "This model's maximum context length is 200000 tokens", ClassContext},
		{"opencode message model", "Model not found: foo/bar", ClassModelUnavailable},
		{"opencode message not found without model text", "Not found", ClassUnknown},
		{"opencode message server", "Service Unavailable", ClassServer},
		{"opencode message overloaded", "Overloaded", ClassServer},
		{"opencode detail fallback", "insufficient_quota", ClassQuota},

		// OpenCode {name,data:{message,statusCode,responseBody}} shape. Hypothesis: not in repo fixtures,
		// and client.go:949-953 drops statusCode before classification. Kept so the parser stays covered.
		{"opencode 429 rate", `{"name":"APIError","data":{"message":"Too Many Requests","statusCode":429}}`, ClassRateLimit},
		{"opencode 429 quota body", `{"name":"APIError","data":{"message":"Error","statusCode":429,"responseBody":"{\"error\":{\"type\":\"insufficient_quota\"}}"}}`, ClassQuota},
		{"opencode 401 auth", `{"name":"APIError","data":{"message":"Unauthorized","statusCode":401}}`, ClassAuth},
		{"opencode 403 auth", `{"name":"APIError","data":{"statusCode":403,"message":"Forbidden"}}`, ClassAuth},
		{"opencode 402 quota", `{"name":"APIError","data":{"message":"Payment required","statusCode":402}}`, ClassQuota},
		{"opencode 413 context", `{"name":"APIError","data":{"message":"Request too large","statusCode":413}}`, ClassContext},
		{"opencode context overflow name", `{"name":"ContextOverflowError","data":{"message":"Input exceeds context window"}}`, ClassContext},
		{"opencode model not found", `{"name":"APIError","data":{"message":"Model not found: foo/bar","statusCode":404}}`, ClassModelUnavailable},
		{"opencode 404 without model text", `{"name":"APIError","data":{"message":"Not found","statusCode":404}}`, ClassUnknown},
		{"opencode 503 server", `{"name":"APIError","data":{"message":"Service unavailable","statusCode":503}}`, ClassServer},
		{"opencode 529 overloaded", `{"name":"APIError","data":{"message":"Overloaded","statusCode":529}}`, ClassServer},
		{"opencode nested error envelope", `{"error":{"name":"APIError","data":{"statusCode":500,"message":"x"}}}`, ClassServer},

		// Claude Code result line (translate.go:262). Hypothesis: api_error_status shape is not in repo fixtures.
		{"claude usage limit", `{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached|1761000000"}`, ClassQuota},
		{"claude hit your limit", `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["You've hit your limit · resets 5pm"]}`, ClassQuota},
		{"claude api 429", `{"type":"result","is_error":true,"api_error_status":429,"result":"API Error: 429"}`, ClassRateLimit},
		{"claude api 401", `{"type":"result","is_error":true,"api_error_status":401,"result":"Invalid API key"}`, ClassAuth},
		{"claude api 400 context", `{"type":"result","is_error":true,"api_error_status":400,"result":"prompt is too long: 250000 tokens > 200000"}`, ClassContext},
		{"claude api 404 model", `{"type":"result","is_error":true,"api_error_status":404,"result":"model: claude-x not found"}`, ClassModelUnavailable},
		{"claude api 500 server", `{"type":"result","is_error":true,"api_error_status":500,"result":"Internal server error"}`, ClassServer},
		{"claude aborted unknown", `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_tools"}`, ClassUnknown},

		// Plain text fallback.
		{"text rate", "Rate limit reached for requests", ClassRateLimit},
		{"text quota", "out of credits, top up to continue", ClassQuota},
		{"text token exhaustion", "tokens exhausted for this month", ClassQuota},
		{"text context", "maximum context length is 200000 tokens", ClassContext},
		{"text auth", "authentication failed", ClassAuth},
		// 400 with auth wording only: the status alone (400) is not auth, the phrase is.
		{"text 400 invalid api key", "400 invalid api key provided", ClassAuth},
		{"text 400 invalid_grant", "400 invalid_grant: refresh token is invalid", ClassAuth},
		{"text 401 auth", "401 Unauthorized", ClassAuth},
		{"text 403 auth", "403 Forbidden", ClassAuth},
		{"text status code 401 auth", "opencode POST /session: status code 401", ClassAuth},
		{"text status code 403 auth", "opencode POST /session: status code 403", ClassAuth},
		{"text model", "model unavailable right now", ClassModelUnavailable},
		{"text status code 503", "opencode POST /x: status code 503", ClassServer},
		{"text crash", "claude code exited unexpectedly", ClassCrash},
		{"text crash exit status", "exit status 2", ClassCrash},
		{"text unknown", "something odd happened", ClassUnknown},
		{"empty unknown", "", ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyText(tt.msg); got != tt.want {
				t.Fatalf("ClassifyText(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestClassifyNilAndError(t *testing.T) {
	if got := Classify(nil); got != ClassUnknown {
		t.Fatalf("Classify(nil) = %q", got)
	}
	if got := Classify(errors.New("Rate limit hit")); got != ClassRateLimit {
		t.Fatalf("Classify(error) = %q", got)
	}
}
