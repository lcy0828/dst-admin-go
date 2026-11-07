package intent

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var (
	leadingAtRe     = regexp.MustCompile(`^(?:@\S+\s*)+`)
	politePrefixRe  = regexp.MustCompile(`^(?:请问|请帮我|请给我|帮我看看|给我看看|麻烦看看|请看看|帮我看下|给我看下|看下|请问一下|能不能帮我|能不能|可以帮我|可以|麻烦帮我|麻烦给我|请|帮我|麻烦|给我)\s*`)
	politeSuffixRe  = regexp.MustCompile(`(?:\s*(?:一下吧|一下呀|一下啊|一下|好吗|可以吗|行吗|谢谢啦|谢谢|吧|呀|啊|呢|喔|哦|啦|[。！？!?～~，,]))+$`)
	multiSpaceRe    = regexp.MustCompile(`\s+`)
	chineseNumberRe = regexp.MustCompile(`[零〇一二两三四五六七八九十百]+`)
)

// Normalize cleans conversational wrappers while preserving entity boundaries.
func Normalize(raw string) string {
	s := strings.TrimSpace(raw)
	s = leadingAtRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	for {
		next := politePrefixRe.ReplaceAllString(s, "")
		if next == s || strings.TrimSpace(next) == "" {
			break
		}
		s = strings.TrimSpace(next)
	}
	s = politeSuffixRe.ReplaceAllString(s, "")
	s = multiSpaceRe.ReplaceAllString(strings.TrimSpace(s), " ")
	return s
}

// Compact returns a lowercase, whitespace-free string for exact/substring matching.
func Compact(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ParseChineseInt parses common Arabic or Chinese numerals (1..999).
func ParseChineseInt(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n, true
	}
	digits := map[rune]int{
		'零': 0, '〇': 0,
		'一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
		'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	}
	runes := []rune(s)
	total := 0
	current := 0
	for _, r := range runes {
		if d, ok := digits[r]; ok {
			current = d
			continue
		}
		switch r {
		case '十':
			if current == 0 {
				current = 1
			}
			total += current * 10
			current = 0
		case '百':
			if current == 0 {
				current = 1
			}
			total += current * 100
			current = 0
		default:
			return 0, false
		}
	}
	total += current
	return total, total > 0 || s == "零" || s == "〇"
}
