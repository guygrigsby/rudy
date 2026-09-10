package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The CLI is noun then verb: `rudy sessions resume`, never `rudy resume-session`. The
// guards here walk the command tree and fail when a new command breaks that shape, so the
// rule is enforced by the build rather than by whoever reviews the diff. ADR 0022.

// topLevelVerbs are the only commands allowed to act without a noun in front of them, each
// for a stated reason. Everything else at the top level is a noun whose subcommands do the
// work.
var topLevelVerbs = map[string]string{
	"serve":      "the daemon itself; there is no noun it acts on",
	"help":       "cobra's own",
	"completion": "cobra's own",
}

// verbs are the subcommand names this CLI uses. A new one has to be added here, which is
// the point: it makes naming a deliberate edit rather than whatever came to mind. Leaf
// names that read as a noun (path, example) are listed with the rest because they are the
// object of an implied show, the way `npm config get` is.
var verbs = []string{
	"add", "disable", "enable", "example", "fork", "get", "install", "list",
	"migrate", "new", "path", "remove", "resume", "show", "sync", "trust", "uninstall",
	"update",
}

// shapeRoot is the command tree as a run builds it. The builder is never called: these
// tests read the tree's shape, not what any command does.
func shapeRoot() *cobra.Command {
	return newRoot("test", func(ctx context.Context, o BuildOptions) (*Built, error) {
		return nil, errors.New("shape test: nothing is built")
	})
}

// TestEveryTopLevelCommandIsANounOrANamedVerb: a bare noun that acts (the old `rudy models`,
// which listed) leaves no room for a second verb and reads differently from every other
// command.
func TestEveryTopLevelCommandIsANounOrANamedVerb(t *testing.T) {
	for _, cmd := range shapeRoot().Commands() {
		name := cmd.Name()
		if reason, ok := topLevelVerbs[name]; ok {
			if len(cmd.Commands()) > 0 {
				t.Errorf("`rudy %s` is listed as a top-level verb (%s) but has subcommands; drop it from topLevelVerbs", name, reason)
			}
			continue
		}
		if len(cmd.Commands()) == 0 {
			t.Errorf("`rudy %s` acts with no verb; give it subcommands, or name it in topLevelVerbs with a reason", name)
		}
		if cmd.Runnable() && cmd.Args == nil {
			t.Errorf("`rudy %s` is a noun, so it should print its help and take no arguments", name)
		}
	}
}

// TestEverySubcommandIsAKnownVerb keeps the vocabulary small and deliberate.
func TestEverySubcommandIsAKnownVerb(t *testing.T) {
	var walk func(parent string, cmd *cobra.Command)
	walk = func(parent string, cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			name := sub.Name()
			if _, top := topLevelVerbs[name]; top && parent == "rudy" {
				continue
			}
			if parent != "rudy" && !slices.Contains(verbs, name) {
				t.Errorf("`%s %s` is not a verb this CLI uses; add it to verbs in shape_test.go if it belongs", parent, name)
			}
			walk(parent+" "+name, sub)
		}
	}
	walk("rudy", shapeRoot())
}

// TestEveryCommandSaysWhatItDoes: a command with no Short is a row of whitespace in the
// help every other command is listed in.
func TestEveryCommandSaysWhatItDoes(t *testing.T) {
	var walk func(path string, cmd *cobra.Command)
	walk = func(path string, cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			if strings.TrimSpace(sub.Short) == "" {
				t.Errorf("`%s %s` has no Short", path, sub.Name())
			}
			walk(path+" "+sub.Name(), sub)
		}
	}
	walk("rudy", shapeRoot())
}

// TestEveryFlagIsLong: long flags take two dashes, and a short flag is one letter, so `-p`
// and `--print` are the same flag and nothing in between exists.
func TestEveryFlagIsLong(t *testing.T) {
	var walk func(path string, cmd *cobra.Command)
	walk = func(path string, cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if len(f.Name) == 1 {
				t.Errorf("`%s --%s` is a one letter long flag; make it a word and give it a shorthand instead", path, f.Name)
			}
			if f.Shorthand != "" && len(f.Shorthand) != 1 {
				t.Errorf("`%s -%s` is a multi letter short flag", path, f.Shorthand)
			}
		})
		for _, sub := range cmd.Commands() {
			walk(path+" "+sub.Name(), sub)
		}
	}
	walk("rudy", shapeRoot())
}
