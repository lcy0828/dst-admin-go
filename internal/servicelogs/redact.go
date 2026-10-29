package servicelogs

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var credentials = regexp.MustCompile(`(?:dstai_[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{16,}|pds-g\^[A-Za-z0-9^_-]+)`)
var secretPair = regexp.MustCompile(`(?i)((?:password|passwd|secret|cluster[_-]?(?:token|key)|security[_-]?key|api[_-]?key|access[_-]?token|refresh[_-]?token|download[_-]?token|token)\s*["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'[^']*'|[^\s,;"'\\}]+)`)
var secretHeader = regexp.MustCompile(`(?im)((?:proxy-authorization|authorization|set-cookie|cookie)["']?\s*[:=]\s*)[^\r\n]+`)
var urls = regexp.MustCompile(`(?i)(?:https?|wss?|socks5h?)://[^\s"'<>]+`)

func Redact(value string) string {
	if value == "" {
		return ""
	}
	value = urls.ReplaceAllStringFunc(value, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED_URL]"
		}
		if u.User != nil {
			u.User = url.User("REDACTED")
		}
		q := u.Query()
		for key := range q {
			k := strings.ToLower(key)
			if strings.Contains(k, "token") || strings.Contains(k, "key") || strings.Contains(k, "password") || strings.Contains(k, "secret") || strings.Contains(k, "signature") {
				q.Set(key, "REDACTED")
			}
		}
		u.RawQuery = q.Encode()
		return u.String()
	})
	value = credentials.ReplaceAllString(value, "[REDACTED]")
	value = secretPair.ReplaceAllString(value, "${1}[REDACTED]")
	return secretHeader.ReplaceAllString(value, "${1}[REDACTED]")
}
func bounded(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value, true
}
