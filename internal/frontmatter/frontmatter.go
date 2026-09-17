// SPDX-License-Identifier: AGPL-3.0-or-later

// Package frontmatter splits a YAML header from a body in a file that opens with a "---"
// fence, the shape shared by agent definitions and skills.
package frontmatter

import (
	"errors"
	"strings"
)

// ErrNoOpeningFence means text does not start with "---\n".
var ErrNoOpeningFence = errors.New("frontmatter: file does not start with a --- fence")

// ErrNoClosingFence means text opens with "---\n" but the fence never closes.
var ErrNoClosingFence = errors.New("frontmatter: no closing --- fence")

// Split separates the YAML frontmatter from the body of text. The text must start with
// "---\n"; the header ends at the next "\n---\n", or at a trailing "\n---" with no body.
// err is ErrNoOpeningFence or ErrNoClosingFence when text does not have that shape.
func Split(text string) (yamlPart, body string, err error) {
	const fence = "---\n"
	if !strings.HasPrefix(text, fence) {
		return "", "", ErrNoOpeningFence
	}
	rest := text[len(fence):]
	if i := strings.Index(rest, "\n"+fence); i >= 0 {
		return rest[:i], rest[i+len("\n"+fence):], nil
	}
	if strings.HasSuffix(rest, "\n---") {
		return strings.TrimSuffix(rest, "\n---"), "", nil
	}
	return "", "", ErrNoClosingFence
}
