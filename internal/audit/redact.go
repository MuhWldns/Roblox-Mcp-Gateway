package audit

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Column bounds mirrored from the audit schema. Free-form fields are
// truncated to these bounds before persistence so an oversized value can
// never fail the append in strict SQL mode.
const (
	maxReasonLength       = 1000
	maxCorrelationLength  = 255
	maxTargetTypeLength   = 128
	maxTargetIDLength     = 255
	maxStateKeyLength     = 128
	maxStateValueLength   = 512
	redactedPlaceholder   = "[redacted]"
	redactedPayloadMarker = "[redacted-payload]"
)

// Secret patterns scrubbed from every free-form audit field. They cover the
// credential shapes this gateway mints and proxies: device credentials,
// enrollment codes, connector tokens, session cookies, bearer headers, and
// database DSNs.
var (
	secretPatterns = []*regexp.Regexp{
		// Roblox-style OIDC tokens (three dot-separated JWT segments).
		regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`),
		// Gateway-minted opaque credentials: device credentials, enrollment
		// user codes, connector access/refresh tokens, and authorize codes.
		regexp.MustCompile(`\b(?:rkd|rkuc|rk13|mca|mcr|mcc)_[A-Za-z0-9._-]{4,}`),
		// Authorization headers and bearer tokens.
		regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*\S+`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
		// Session and CSRF cookie lines, including Set-Cookie responses.
		regexp.MustCompile(`(?i)\bcookies?\s*[:=][^\r\n]*`),
		regexp.MustCompile(`(?i)\bset-cookie\s*:[^\r\n]*`),
		regexp.MustCompile(`__Host-[A-Za-z0-9_-]+=[A-Za-z0-9._~+/=-]+`),
		// PKCE verifiers presented inline.
		regexp.MustCompile(`(?i)\bcode_verifier\s*[:=]\s*\S+`),
		// MySQL DSNs and URLs with embedded user credentials.
		regexp.MustCompile(`[A-Za-z0-9._%+~-]+:[^@\s/]+@tcp\([^\s)]*\)[^\s]*`),
		regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^/\s:@]+:[^@\s/]+@[^\s]*`),
	}
	// sensitiveKeyFragments mark metadata/state keys whose value is treated
	// as a secret wholesale.
	sensitiveKeyFragments = []string{
		"authorization", "cookie", "token", "secret", "password", "passwd",
		"verifier", "credential", "dsn", "session", "bearer", "apikey",
		"api_key", "code",
	}
)

// RedactString scrubs one free-form value: raw JSON payloads are replaced
// wholesale, every known secret pattern is masked, and the result is
// truncated to maxRunes on a rune boundary.
func RedactString(value string, maxRunes int) string {
	if value == "" {
		return ""
	}
	value = removeJSONPayloads(value)
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllString(value, redactedPlaceholder)
	}
	return truncateRunes(value, maxRunes)
}

// Redact returns a secret-free copy of the event. Structured identifiers
// (actor, user, action, timestamps) pass through; free-form fields and the
// before/after state maps are scrubbed and bounded. The returned event
// never shares maps with the input.
func Redact(event Event) Event {
	out := event
	out.CorrelationID = RedactString(event.CorrelationID, maxCorrelationLength)
	out.Reason = RedactString(event.Reason, maxReasonLength)
	out.TargetType = RedactString(event.TargetType, maxTargetTypeLength)
	out.TargetID = RedactString(event.TargetID, maxTargetIDLength)
	out.Before = redactStateMap(event.Before)
	out.After = redactStateMap(event.After)
	return out
}

func redactStateMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		key = truncateRunes(key, maxStateKeyLength)
		if sensitiveKey(key) {
			out[key] = redactedPlaceholder
			continue
		}
		out[key] = RedactString(value, maxStateValueLength)
	}
	return out
}

// sensitiveKey reports whether a state key names something secret-like, so
// its value is redacted in place regardless of its content.
func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

// removeJSONPayloads replaces every embedded JSON object or array with the
// payload marker. Audit free-form fields are prose and enumerated codes;
// they never legitimately carry structured payloads, so a parseable one is
// dropped wholesale.
func removeJSONPayloads(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); {
		c := value[i]
		if c != '{' && c != '[' {
			out.WriteByte(c)
			i++
			continue
		}
		end := matchingBracket(value, i)
		if end < 0 || !isJSONValue(value[i:end+1]) {
			out.WriteByte(c)
			i++
			continue
		}
		out.WriteString(redactedPayloadMarker)
		i = end + 1
	}
	return out.String()
}

// matchingBracket returns the index of the bracket closing value[start],
// honoring JSON string escapes; -1 when the bracket never closes.
func matchingBracket(value string, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(value); i++ {
		c := value[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// isJSONValue reports whether the candidate parses as JSON. A parseable
// value inside a free-form audit field is treated as a smuggled payload.
func isJSONValue(candidate string) bool {
	trimmed := strings.TrimSpace(candidate)
	if len(trimmed) < 2 {
		return false
	}
	return json.Valid([]byte(trimmed))
}

func truncateRunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}
