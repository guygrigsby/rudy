// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	memory "github.com/aeryx-ai/memory/memory-go"
	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/config"
)

// setupProvider is one first connection the wizard knows how to describe. Name is the
// [providers.<name>] table it becomes and Model is only the suggestion shown in the
// prompt: the operator's own id always wins.
type setupProvider struct {
	Name    string
	Wire    string
	BaseURL string
	Auth    string
	Model   string
}

// setupPresets are the providers a fresh install is asked about, in the order the menu
// lists them. Anything else is "custom", which the wizard asks about by hand.
var setupPresets = []setupProvider{
	{Name: "anthropic", Wire: "anthropic_messages", BaseURL: "https://api.anthropic.com", Auth: "env:ANTHROPIC_API_KEY", Model: "claude-sonnet-4-5"},
	{Name: "openai", Wire: "openai_chat", BaseURL: "https://api.openai.com/v1", Auth: "env:OPENAI_API_KEY", Model: "gpt-5.2"},
}

// setupOptions is what `rudy setup` was told on the command line. Provider non-empty is
// the non-interactive form: every question is answered by a flag and nothing is read.
type setupOptions struct {
	Env  func(string) string // nil means os.Getenv
	Home string              // "" means os.UserHomeDir

	Provider string // a preset name, or the name a custom provider takes with --wire and --base-url
	Wire     string
	BaseURL  string
	Auth     string // env:NAME, cache:KEY, a raw key to cache, or "" for none
	Model    string
}

// newSetupCmd is `rudy setup`: the first-run wizard. A fresh install used to stop at
// three errors naming three files; this is the walk through them instead.
func newSetupCmd() *cobra.Command {
	var o setupOptions
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "first-run wizard: write config.toml, init memory and connect a model provider",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetup(o, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Provider, "provider", "", "configure this provider without prompting (anthropic, openai or a custom name with --wire and --base-url)")
	f.StringVar(&o.Wire, "wire", "", "openai_chat, anthropic_messages or custom, for a custom --provider")
	f.StringVar(&o.BaseURL, "base-url", "", "the endpoint, for a custom --provider")
	f.StringVar(&o.Auth, "auth", "", "env:NAME or cache:KEY; a raw key is written to secrets.file and referenced cache:KEY")
	f.StringVar(&o.Model, "model", "", "the model id a new session opens on")
	return cmd
}

// runSetup is the wizard itself. Every step is idempotent: the file sync adds only what
// is missing, the memory bundle is initialized only when absent, and a provider is
// configured only when none is. Re-running it is how a half-finished first run resumes.
func runSetup(o setupOptions, in io.Reader, out, stderr io.Writer) error {
	env, home, err := envAndHome(BuildOptions{Env: o.Env, Home: o.Home})
	if err != nil {
		return err
	}
	paths := config.XDG(env, home)
	file := paths.ConfigFile()

	res, err := config.Sync(file, false)
	if err != nil {
		return err
	}
	switch {
	case res.Created:
		_, _ = fmt.Fprintf(out, "wrote %s with every key and what each is for\n", res.Path)
	case len(res.Added) > 0:
		_, _ = fmt.Fprintf(out, "added %d missing keys to %s\n", len(res.Added), res.Path)
	default:
		_, _ = fmt.Fprintf(out, "%s has every key\n", res.Path)
	}

	cfg, err := config.Load(paths, nil)
	if err != nil {
		return err
	}
	if cfg.Memory.Enabled {
		b := memory.New(memory.ResolveRoot(cfg.Memory.Dir, env))
		if !b.Exists() {
			if err := b.Init("", time.Now()); err != nil {
				_, _ = fmt.Fprintf(out, "memory: no bundle at %s and it could not be initialized: %v\n", b.Root, err)
			} else {
				_, _ = fmt.Fprintf(out, "memory: initialized a bundle at %s\n", b.Root)
			}
		}
	}

	if len(cfg.Providers) > 0 && o.Provider == "" {
		_, _ = fmt.Fprintln(out, "a model provider is already configured; nothing left to do")
		return nil
	}
	p, ok, err := setupPickProvider(o, in, out)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintf(out, "no provider configured; add a [providers.<name>] table to %s when you have one\n", file)
		return nil
	}
	if p.Auth != "" && !strings.HasPrefix(p.Auth, "env:") && !strings.HasPrefix(p.Auth, "cache:") {
		// A pasted key is a value, not a reference: it goes in secrets.file and the
		// config carries the cache:KEY reference, so the file rudy logs about never
		// holds the key itself.
		key := secretKeyFor(p.Name)
		if err := storeSecret(cfg.Secrets.File, key, p.Auth); err != nil {
			return err
		}
		p.Auth = "cache:" + key
		_, _ = fmt.Fprintf(out, "wrote the key to %s; config.toml references it as cache:%s\n", cfg.Secrets.File, key)
	}
	if p.Model == "" {
		model, err := askRequired(bufio.NewReader(in), out, "default model id", "")
		if err != nil {
			return err
		}
		p.Model = model
	}
	if err := writeProvider(file, p); err != nil {
		return err
	}
	// The file was just written by hand: prove rudy reads it back before saying it is
	// ready, so a table the validator refuses is the wizard's failure, not the next
	// start's.
	if _, err := config.Load(paths, nil); err != nil {
		return fmt.Errorf("the provider was written but config.toml no longer loads: %w", err)
	}
	_, _ = fmt.Fprintf(out, "configured %s (%s, default model %s) in %s\nrudy is ready; start it with `rudy`\n", p.Name, p.Wire, p.Model, file)
	return nil
}

// setupPickProvider answers the one question the wizard exists for: which provider the
// first connection is with. Flags answer it outright; otherwise the presets are listed
// and the answer is read. ok false is an operator who looked and said not yet.
func setupPickProvider(o setupOptions, in io.Reader, out io.Writer) (setupProvider, bool, error) {
	if o.Provider != "" {
		for _, preset := range setupPresets {
			if o.Provider == preset.Name {
				p := preset
				if o.Auth != "" {
					p.Auth = o.Auth
				}
				if o.Model != "" {
					p.Model = o.Model
				}
				return p, true, nil
			}
		}
		if o.BaseURL == "" {
			return setupProvider{}, false, fmt.Errorf("--provider %q is not a preset; give it --base-url (and --wire when it is not openai_chat)", o.Provider)
		}
		wire := o.Wire
		if wire == "" {
			wire = "openai_chat"
		}
		return setupProvider{Name: o.Provider, Wire: wire, BaseURL: o.BaseURL, Auth: o.Auth, Model: o.Model}, true, nil
	}
	r := bufio.NewReader(in)
	_, _ = fmt.Fprintln(out, "\nno model provider is configured yet. Which should rudy connect first?")
	for i, preset := range setupPresets {
		_, _ = fmt.Fprintf(out, "  %d) %s (%s)\n", i+1, preset.Name, preset.BaseURL)
	}
	_, _ = fmt.Fprintf(out, "  %d) another endpoint (openai_chat compatible)\n", len(setupPresets)+1)
	_, _ = fmt.Fprintf(out, "  %d) not yet\n", len(setupPresets)+2)
	choice, err := askRequired(r, out, fmt.Sprintf("choice [1-%d]", len(setupPresets)+2), "1")
	if err != nil {
		return setupProvider{}, false, err
	}
	switch choice {
	case "1", "2":
		p := setupPresets[choice[0]-'1']
		auth, err := askRequired(r, out, fmt.Sprintf("API key reference, a pasted key, or empty for none [%s]", p.Auth), p.Auth)
		if err != nil {
			return setupProvider{}, false, err
		}
		p.Auth = auth
		model, err := askRequired(r, out, fmt.Sprintf("default model [%s]", p.Model), p.Model)
		if err != nil {
			return setupProvider{}, false, err
		}
		p.Model = model
		return p, true, nil
	default:
		if choice == fmt.Sprint(len(setupPresets)+1) {
			name, err := askRequired(r, out, "provider name (the [providers.<name>] table)", "")
			if err != nil {
				return setupProvider{}, false, err
			}
			base, err := askRequired(r, out, "base URL, ending with /v1", "")
			if err != nil {
				return setupProvider{}, false, err
			}
			auth, err := askRequired(r, out, "API key reference, a pasted key, or empty for none", "")
			if err != nil {
				return setupProvider{}, false, err
			}
			model, err := askRequired(r, out, "default model id", "")
			if err != nil {
				return setupProvider{}, false, err
			}
			return setupProvider{Name: name, Wire: "openai_chat", BaseURL: base, Auth: auth, Model: model}, true, nil
		}
		return setupProvider{}, false, nil
	}
}

// askRequired reads one line behind the prompt and answers def when the line is empty.
// It loops rather than return empty when there is no default: a name, a URL and a model
// are not optional.
func askRequired(r *bufio.Reader, out io.Writer, prompt, def string) (string, error) {
	for {
		_, _ = fmt.Fprintf(out, "%s: ", prompt)
		line, err := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if err != nil && line == "" {
			return "", fmt.Errorf("setup needs an answer for %q; %w", prompt, err)
		}
		if line != "" {
			return line, nil
		}
		if def != "" {
			return def, nil
		}
	}
}

// offerSetup asks the operator at the terminal whether the first-run wizard should run
// now, the question runTUI puts in place of the no-provider error. The reader is shared
// with the wizard that follows, so neither one's buffered read eats the other's line.
func offerSetup(r *bufio.Reader, out io.Writer) bool {
	_, _ = fmt.Fprint(out, "rudy: no model provider is configured yet. Run first-run setup now? [Y/n] ")
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// secretKeyFor is the secrets.file key a provider's pasted key is stored under.
func secretKeyFor(providerName string) string {
	key := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			return r
		}
		return '_'
	}, providerName)
	return key + "_API_KEY"
}

// storeSecret appends KEY=value to the env-format file secrets.file names, unless the
// key is already there. The file is operator-owned like config.toml, so an existing key
// is left exactly as it is and the wizard says it was kept.
func storeSecret(path, key, value string) error {
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secrets: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if k, _, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "export ")), "="); ok && strings.TrimSpace(k) == key {
			return fmt.Errorf("secrets: %s already has %s; leaving it as it is, reference it with cache:%s", path, key, key)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintf(f, "%s=%s\n", key, value); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	return nil
}

// writeProvider adds the [providers.<name>] table and points default.provider and
// default.model at it. The edit is sync's own rule: text, not a re-marshal, and nothing
// already in the file changes but the two [default] keys this table exists to fill,
// which are empty on every first run that reaches here.
func writeProvider(file string, p setupProvider) error {
	body, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	text := string(body)
	if hasTable(text, "providers."+p.Name) {
		return fmt.Errorf("setup: [providers.%s] is already in %s; edit it there", p.Name, file)
	}
	var block strings.Builder
	block.WriteString("\n[providers." + p.Name + "]\n")
	block.WriteString("wire = " + tomlQuote(p.Wire) + "\n")
	block.WriteString("base_url = " + tomlQuote(p.BaseURL) + "\n")
	if p.Auth != "" {
		block.WriteString("auth = " + tomlQuote(p.Auth) + "\n")
	}
	text = strings.TrimRight(text, "\n") + "\n" + block.String()
	text = setTableValues(text, "default", map[string]string{"provider": p.Name, "model": p.Model}, []string{"provider", "model"})
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	return nil
}

// tomlQuote is a quoted basic string: backslash and the quote itself are the only
// escapes a name, URL or reference can carry that need one.
func tomlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// hasTable reports whether body carries the TOML table header, uncommented.
func hasTable(body, table string) bool {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") && !strings.HasPrefix(t, "[[") && t[1:len(t)-1] == table {
			return true
		}
	}
	return false
}

// setTableValues replaces the values of the named keys inside one TOML table, inserting
// the keys the table lacks right under its header. A missing table is appended whole.
// Comments, ordering and every other line are byte-for-byte what they were.
func setTableValues(body, table string, values map[string]string, order []string) string {
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") || strings.HasPrefix(t, "[[") {
			continue
		}
		if start >= 0 {
			end = i
			break
		}
		if t[1:len(t)-1] == table {
			start = i
		}
	}
	if start < 0 {
		block := "[" + table + "]\n"
		for _, k := range order {
			block += k + " = " + tomlQuote(values[k]) + "\n"
		}
		return strings.TrimRight(body, "\n") + "\n\n" + block
	}
	done := map[string]bool{}
	for i := start + 1; i < end; i++ {
		for k, v := range values {
			if keyLine(lines[i], k) {
				lines[i] = k + " = " + tomlQuote(v)
				done[k] = true
			}
		}
	}
	var missing []string
	for _, k := range order {
		if !done[k] {
			missing = append(missing, k+" = "+tomlQuote(values[k]))
		}
	}
	out := append([]string{}, lines[:start+1]...)
	out = append(out, missing...)
	out = append(out, lines[start+1:]...)
	return strings.Join(out, "\n") + "\n"
}

// keyLine reports whether line assigns the bare key: uncommented, the key first, then
// the '='. Indented assignments under the table count; commented-out ones do not.
func keyLine(line, key string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") || !strings.HasPrefix(t, key) {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(t, key))
	return strings.HasPrefix(rest, "=")
}
