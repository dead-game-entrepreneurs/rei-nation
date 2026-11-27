package tsfp

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

func token(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func fields(f aries.Frame) map[string]string {
	out := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimRight(f.Body, "\x00"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		out[strings.ToUpper(key)] = strings.ToValidUTF8(value, "\uFFFD")
	}
	return out
}

func encode(value string) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for i := range len(value) {
		c := value[i]
		if c < 32 || c >= 127 || strings.ContainsRune("=\":%^", rune(c)) {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func packet(tag string, pairs ...string) aries.Frame {
	f := aries.Reply(tag, nil)
	var body strings.Builder
	for i := 0; i < len(pairs); i += 2 {
		body.WriteString(pairs[i])
		body.WriteByte('=')
		value := encode(pairs[i+1])
		if strings.ContainsRune(value, ' ') {
			body.WriteByte('"')
			body.WriteString(value)
			body.WriteByte('"')
		} else {
			body.WriteString(value)
		}
		body.WriteByte('\n')
	}
	f.Body = body.String()
	return f
}

func appendFields(pairs []string, f map[string]string) []string {
	keys := make([]string, 0, len(f))
	for key := range f {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pairs = append(pairs, key, f[key])
	}
	return pairs
}

func decimal(s string, max int) (int, bool) {
	if !transactionID(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n <= max
}

func transactionID(s string) bool {
	return len(s) > 0 && len(s) <= 10 && !strings.ContainsFunc(s, func(c rune) bool { return c < '0' || c > '9' })
}

func fallback(fields map[string]string, key, defaultValue string) string {
	if value, ok := fields[key]; ok {
		return value
	}
	return defaultValue
}
