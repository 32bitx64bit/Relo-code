package logging

import (
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)((?:password|authorization|opencode_server_password)\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`),
	regexp.MustCompile(`(?i)(authorization\s*:\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`),
}

type Level string

const (
	Debug Level = "debug"
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	level Level
	base  map[string]any
}

func New(out io.Writer, level Level) *Logger {
	if out == nil {
		out = os.Stderr
	}
	return &Logger{out: out, level: level, base: map[string]any{}}
}

func (l *Logger) With(fields map[string]any) *Logger {
	next := map[string]any{}
	for k, v := range l.base {
		next[k] = v
	}
	for k, v := range fields {
		next[k] = v
	}
	return &Logger{out: l.out, level: l.level, base: next}
}

func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

func (l *Logger) Debug(msg string, fields map[string]any) { l.write(Debug, msg, fields) }
func (l *Logger) Info(msg string, fields map[string]any)  { l.write(Info, msg, fields) }
func (l *Logger) Warn(msg string, fields map[string]any)  { l.write(Warn, msg, fields) }
func (l *Logger) Error(msg string, fields map[string]any) { l.write(Error, msg, fields) }

func (l *Logger) write(level Level, msg string, fields map[string]any) {
	if !enabled(l.level, level) {
		return
	}
	row := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"level": level,
		"msg":   msg,
	}
	l.mu.Lock()
	for k, v := range l.base {
		if !secretKey(k) {
			row[k] = redact(v)
		}
	}
	out := l.out
	l.mu.Unlock()
	for k, v := range fields {
		if secretKey(k) {
			continue
		}
		row[k] = redact(v)
	}
	row["msg"] = redactString(msg)
	b, err := json.Marshal(row)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = out.Write(append(b, '\n'))
}

func secretKey(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", ""), "-", ""))
	return k == "secret" || k == "password" || k == "token" || k == "key" || k == "authorization" || k == "opencodeserverpassword"
}

func redact(v any) any {
	switch x := v.(type) {
	case string:
		return redactString(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if !secretKey(k) {
				out[k] = redact(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redact(value)
		}
		return out
	default:
		return v
	}
}

func redactString(s string) string {
	for _, re := range secretPatterns {
		if re.NumSubexp() > 0 {
			s = re.ReplaceAllString(s, `${1}[REDACTED]`)
		} else {
			s = re.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return s
}

func enabled(min, got Level) bool {
	order := map[Level]int{Debug: 0, Info: 1, Warn: 2, Error: 3}
	return order[got] >= order[min]
}

func ParseLevel(s string) Level {
	switch Level(s) {
	case Debug, Info, Warn, Error:
		return Level(s)
	default:
		return Info
	}
}
