// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"regexp"
	"strings"
	"sync"
)

var (
	authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)\S+`)
	cookiePattern        = regexp.MustCompile(`(?i)(cookie\s*:\s*)[^\r\n]+`)
	urlPattern           = regexp.MustCompile(`https?://[^\s]+`)
	secretValuePattern   = regexp.MustCompile(`(?i)\b(code|token|access_token|refresh_token|client_secret)=([^&\s]+)`)
	jsonSecretPattern    = regexp.MustCompile(`(?i)("(?:authorization|cookie|code|token|access_token|refresh_token|client_secret)"\s*:\s*)"(?:\\.|[^"\\])*"`)
)

// Redact strips credentials and URL query values from App Server diagnostics.
func Redact(value string) string {
	value = authorizationPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = cookiePattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = urlPattern.ReplaceAllStringFunc(value, func(raw string) string {
		if i := strings.IndexByte(raw, '?'); i >= 0 {
			return raw[:i] + "?[REDACTED]"
		}
		return raw
	})
	value = secretValuePattern.ReplaceAllString(value, `${1}=[REDACTED]`)
	return jsonSecretPattern.ReplaceAllString(value, `${1}"[REDACTED]"`)
}

type diagnosticTail struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func newDiagnosticTail(max int) *diagnosticTail { return &diagnosticTail{max: max} }

func (t *diagnosticTail) add(line string) {
	b := []byte(Redact(line))
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, b...)
	t.data = append(t.data, '\n')
	if len(t.data) > t.max {
		t.data = append([]byte(nil), t.data[len(t.data)-t.max:]...)
	}
}

func (t *diagnosticTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}
