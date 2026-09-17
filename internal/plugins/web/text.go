package web

import (
	"strings"

	"golang.org/x/net/html"
)

// extract turns a response body into the title and text a model reads. HTML is reduced here
// rather than handed over as markup: markup is most of the bytes and none of the meaning,
// and a model asked to read tags pays for them twice, once in context and once in attention.
// Anything that is not HTML comes back as it arrived, which is what a plain text page, a
// README or a JSON endpoint should do.
func extract(contentType string, body []byte) (title, text string) {
	if !isHTML(contentType, body) {
		return "", strings.TrimSpace(string(body))
	}
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", strings.TrimSpace(string(body))
	}
	var b strings.Builder
	walk(doc, &title, &b)
	return strings.TrimSpace(title), collapse(b.String())
}

// isHTML decides from the declared type first and the bytes second, because a server that
// mislabels HTML as text/plain is common and a page of tags is not what a model wants.
func isHTML(contentType string, body []byte) bool {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	switch strings.TrimSpace(strings.ToLower(contentType)) {
	case "text/html", "application/xhtml+xml":
		return true
	}
	head := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 512)])))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

// walk collects the document's title and its readable text. script, style, noscript and
// template hold code and fallbacks rather than content, so their text is dropped; a block
// element ends with a newline so paragraphs do not run into each other.
func walk(n *html.Node, title *string, b *strings.Builder) {
	if n.Type == html.ElementNode {
		switch n.Data {
		case "script", "style", "noscript", "template", "svg", "iframe":
			return
		case "title":
			if *title == "" && n.FirstChild != nil {
				*title = n.FirstChild.Data
			}
			return
		}
	}
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, title, b)
	}
	if n.Type == html.ElementNode && block(n.Data) {
		b.WriteString("\n")
	}
}

// block is the elements whose end is a line break in the text.
func block(tag string) bool {
	switch tag {
	case "p", "div", "section", "article", "header", "footer", "main", "aside", "nav",
		"h1", "h2", "h3", "h4", "h5", "h6", "li", "tr", "br", "pre", "blockquote", "table":
		return true
	}
	return false
}

// collapse squeezes the whitespace HTML leaves behind: runs of spaces and tabs become one
// space, runs of blank lines become one, and every line loses its edges.
func collapse(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
		if line == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
