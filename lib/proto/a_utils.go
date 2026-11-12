package proto

import (
	"regexp"
	"strings"
)

// PatternToReg converts a Fetch URLPattern glob to a regular expression.
// Metacharacters other than * and ? are escaped, so a pattern like
// "*/a+b/*" matches the same URLs Chrome's glob matches.
func PatternToReg(pattern string) string {
	if pattern == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`\A`)
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '\\':
			if i+1 < len(rs) {
				i++
				b.WriteString(regexp.QuoteMeta(string(rs[i])))
			} else {
				b.WriteString(`\\`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		}
	}
	b.WriteString(`\z`)
	return b.String()
}
