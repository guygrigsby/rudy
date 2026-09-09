// Package frontmatter splits a YAML header from a body in a file that opens with a "---"
// fence, the shape shared by agent definitions and skills.
package frontmatter

import "strings"

// Split separates the YAML frontmatter from the body of text. The text must start with
// "---\n"; the header ends at the next "\n---\n", or at a trailing "\n---" with no body. ok
// is false when text has no opening fence, or the fence never closes.
func Split(text string) (yamlPart, body string, ok bool) {
	const fence = "---\n"
	if !strings.HasPrefix(text, fence) {
		return "", "", false
	}
	rest := text[len(fence):]
	if i := strings.Index(rest, "\n"+fence); i >= 0 {
		return rest[:i], rest[i+len("\n"+fence):], true
	}
	if strings.HasSuffix(rest, "\n---") {
		return strings.TrimSuffix(rest, "\n---"), "", true
	}
	return "", "", false
}
