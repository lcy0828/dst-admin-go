package dstruntime

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var modNoticeToken = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// ValidModNotice rejects arbitrary payloads, markup instructions and metadata.
// Mod code itself is trusted local server code; this is a bounded text channel,
// not permission to invoke a command or choose a notification category.
func ValidModNotice(fields map[string]any) bool {
	if len(fields) != 3 {
		return false
	}
	source, ok := fields["source"].(string)
	if !ok || !modNoticeToken.MatchString(source) {
		return false
	}
	code, ok := fields["code"].(string)
	if !ok || !modNoticeToken.MatchString(code) {
		return false
	}
	text, ok := fields["message"].(string)
	if !ok || len(text) > 1024 || !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
