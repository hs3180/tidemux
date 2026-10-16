package diagnostics

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bBearer\s+[^\s,;"']+`),
	regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	regexp.MustCompile(`(?i)\b(?:api[-_ ]?key|access[-_ ]?token|authorization|account[-_ ]?(?:id|email))\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
}

var ansiPattern = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")

func credentialShaped(value string) bool {
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

// Redaction precedes truncation so a credential spanning the limit cannot leak
// a prefix. The caller removes known secrets before calling Normalize as well.
func redactMessageShapes(message string) string {
	for _, pattern := range credentialPatterns {
		message = pattern.ReplaceAllString(message, "[redacted]")
	}
	return message
}

func cleanMessage(message string) string {
	message = redactMessageShapes(message)
	message = ansiPattern.ReplaceAllString(message, "")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > 512 {
		message = string(runes[:511]) + "…"
	}
	return message
}

var tokenComparison = regexp.MustCompile(`(?i)\b([0-9]+)\s*tokens?\s*>\s*([0-9]+)\b`)
var ambiguousNumber = regexp.MustCompile(`[0-9][.,][0-9]|[-+][0-9]`)
var contextComparison = regexp.MustCompile(`(?i)\bmaximum context length is ([0-9]+) tokens\b[^\n.]*\brequested ([0-9]+) tokens\b`)

// TokenFacts accepts only explicit token comparisons with a greater actual
// count. Conflicting comparisons, decimals, overflow and implied limits remain
// unknown. These are request facts, never model capability overrides.
func TokenFacts(message string) (actual, limit int64) {
	add := func(a, l string) bool {
		av, ae := strconv.ParseInt(a, 10, 64)
		lv, le := strconv.ParseInt(l, 10, 64)
		if ae != nil || le != nil || lv <= 0 || av <= lv || actual != 0 && (actual != av || limit != lv) {
			return false
		}
		actual, limit = av, lv
		return true
	}
	// Do not parse a numeric suffix of a decimal, signed or grouped count.
	if ambiguousNumber.MatchString(message) {
		return 0, 0
	}
	for _, match := range tokenComparison.FindAllStringSubmatch(message, -1) {
		if !add(match[1], match[2]) {
			return 0, 0
		}
	}
	for _, match := range contextComparison.FindAllStringSubmatch(message, -1) {
		if !add(match[2], match[1]) {
			return 0, 0
		}
	}
	return actual, limit
}
