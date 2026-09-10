package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/guygrigsby/rudy/internal/session"
)

// scopeFile is where the models ctrl+p and ctrl+n cycle through are remembered, under the
// data root beside the plugin trust and lock files. It is the client's own state, chosen
// through /scoped-models rather than written by hand, so it is not config.toml's: that
// file is the operator's and nothing but `rudy config sync` writes it (ADR 0021, ADR 0027).
const scopeFile = "scope.toml"

// scopeDoc is scope.toml: the refs in the order the cycle walks them.
type scopeDoc struct {
	Models []string `toml:"models"`
}

// readScope is the remembered scope, empty when nobody has chosen one. A file that will
// not parse is an error rather than an empty scope: somebody chose those models, and
// forgetting the choice quietly is worse than saying the file is broken.
func readScope(dir string) ([]session.ModelRef, error) {
	body, err := os.ReadFile(filepath.Join(dir, scopeFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rudy: read %s: %w", filepath.Join(dir, scopeFile), err)
	}
	var doc scopeDoc
	if err := toml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("rudy: parse %s: %w", filepath.Join(dir, scopeFile), err)
	}
	out := make([]session.ModelRef, 0, len(doc.Models))
	for _, s := range doc.Models {
		// A spelling that is no longer a ref is dropped rather than refused: the cycle
		// already drops a model the registry has stopped carrying, and one bad line is
		// not a reason to lose the rest of the choice.
		if ref, ok := session.ParseModelRef(s); ok {
			out = append(out, ref)
		}
	}
	return out, nil
}

// writeScope records a choice, an empty one included: clearing the scope is a choice to
// cycle the whole registry, and it has to survive the client the same way.
func writeScope(dir string, refs []session.ModelRef) error {
	doc := scopeDoc{Models: make([]string, 0, len(refs))}
	for _, r := range refs {
		doc.Models = append(doc.Models, r.String())
	}
	body, err := toml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("rudy: write %s: %w", scopeFile, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("rudy: %w", err)
	}
	path := filepath.Join(dir, scopeFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("rudy: write %s: %w", path, err)
	}
	return nil
}
