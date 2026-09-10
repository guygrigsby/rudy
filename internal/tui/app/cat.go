package app

import (
	"math/rand/v2"
	"strings"

	"github.com/guygrigsby/rudy/internal/cats"
)

// pickCat is the face this client wears for the rest of its run, drawn in the status
// line's cat cell. One face per run rather than one per frame: a status line that changed
// its face while somebody typed would be motion where the design puts none.
//
// A face carrying the replacement character is skipped. internal/cats holds a few that
// were mangled on their way into the file, and a box in the status line is not a cat.
func pickCat() string {
	usable := make([]string, 0, len(cats.Cats))
	for _, c := range cats.Cats {
		if c != "" && !strings.ContainsRune(c, '�') {
			usable = append(usable, c)
		}
	}
	if len(usable) == 0 {
		return ""
	}
	return usable[rand.IntN(len(usable))]
}
