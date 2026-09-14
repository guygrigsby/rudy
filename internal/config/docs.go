package config

import (
	"fmt"
	"strings"
)

// This file is the one place a config key is described. Defaults() says what a key is worth
// when nobody set it; Sections below says what it is for and what else it could be, and
// both the example config this repository ships and `rudy config sync` are written from it.
// A key in one and not the other fails a test in this package rather than reaching a
// reader (see drift_test.go). ADR 0021.

// Doc is one key: its name relative to its table, the sentence above it in a config file,
// and an example worth showing beside the default.
type Doc struct {
	// Key is the full dotted key, which is what Defaults() and viper call it.
	Key string
	// Comment is the line written above it, without the leading hash.
	Comment string
	// Example is an alternative value, written after the key as "# e.g. <example>". Empty
	// when the default says everything.
	Example string
}

// Section is one table in a config file: its header, the sentence above the header, and
// the keys under it in the order they are written.
type Section struct {
	// Table is the TOML header without brackets, empty for the keys above every table.
	Table string
	// Comment is the paragraph above the table header, one line per element.
	Comment []string
	Keys    []Doc
	// Sample is an example block written commented out under the table, for the tables
	// whose keys are a user's own names rather than a fixed set (providers, keys, themes).
	// A sample is never written by sync: it is prose, not a default.
	Sample []string
}

// Sections is every table a config file may carry, in the order an example writes them.
// Top-level keys come first because TOML puts a bare key above the first table header.
var Sections = []Section{
	{
		Comment: []string{
			"rudy configuration.",
			"",
			"Every key below is written with the value rudy uses when it is absent, so a file",
			"with all of them changes nothing. Delete what you do not care about, or leave it:",
			"`rudy config sync` adds keys that are missing and never touches a value you set.",
			"",
			"The full table, with the legal values for each key, is docs/specs/rudy-contracts.md.",
		},
		Keys: []Doc{
			{Key: "agent", Comment: "The agent definition a session runs under."},
			{Key: "max_tokens", Comment: "Output tokens a single request may produce."},
			{Key: "hook_timeout_ms", Comment: "How long one hook handler may take before it is abandoned."},
			{Key: "tool_timeout_ms", Comment: "How long one tool invocation may take.", Example: "60000"},
		},
	},
	{
		Table: "log",
		Comment: []string{
			"Where the kernel writes its trail: JSON lines, one per record, appended for the",
			"life of the process. A daemon and a client that attaches to it are two processes",
			"writing one file, so every record carries the pid that wrote it.",
		},
		Keys: []Doc{
			{Key: "log.level", Comment: "debug, info, warn or error. debug adds each tool invocation and provider stream error."},
			{Key: "log.file", Comment: "Empty means $XDG_CACHE_HOME/rudy/rudy.log. A file that cannot be opened is a notice, and the trail goes to stderr instead.", Example: `"~/rudy.log"`},
		},
	},
	{
		Table: "remote",
		Comment: []string{
			"Run the kernel on another machine over ssh, with this terminal as the client.",
			"--host on the command line overrides host. Sessions, plugins, memory and the",
			"provider keys are that machine's; only the terminal is here (ADR 0029).",
		},
		Keys: []Doc{
			{Key: "remote.host", Comment: "ssh alias or user@host. Empty means the kernel runs here.", Example: `"box"`},
			{Key: "remote.source", Comment: "The rudy checkout on the host; rudy hosts install builds from it at this binary's commit. A path on the host: ~ expands there, not here."},
		},
	},
	{
		Table:   "default",
		Comment: []string{"The provider and model a new session opens on."},
		Keys: []Doc{
			{Key: "default.provider", Comment: "A name from [providers.<name>] below.", Example: `"aperture"`},
			{Key: "default.model", Comment: "A model that provider serves, as its own id.", Example: `"cline-pass/kimi-k3"`},
			{Key: "default.thinking", Comment: "off, low, medium or high.", Example: `"medium"`},
		},
	},
	{
		Table: "prompt",
		Comment: []string{
			"The system prompt. Empty reads system.md under this directory when it exists,",
			"and the built-in template when it does not. `rudy prompt example` prints the",
			"built-in one to start from, `rudy prompt show` prints what a session would send.",
		},
		Keys: []Doc{
			{Key: "prompt.file", Comment: "A prompt template of your own.", Example: `"~/.config/rudy/system.md"`},
		},
	},
	{
		Table: "permissions",
		Comment: []string{
			"When rudy asks before running an unsafe tool.",
		},
		Keys: []Doc{
			{Key: "permissions.mode", Comment: "strict asks, permissive allows but still asks for the dangerous set, off allows everything.", Example: `"permissive"`},
			{Key: "permissions.double_press_ms", Comment: "The window two Escapes have to fall inside to cancel a turn."},
			{Key: "permissions.dangerous", Comment: "Command prefixes that ask every time unless the mode is off."},
		},
	},
	{
		Table:   "sessions",
		Comment: []string{"Where session logs live and when they are compacted."},
		Keys: []Doc{
			{Key: "sessions.dir", Comment: "Empty means $XDG_DATA_HOME/rudy/sessions.", Example: `"~/work/rudy-sessions"`},
			{Key: "sessions.compact_at", Comment: "Fraction of the context window that triggers a compaction.", Example: "0.9"},
		},
	},
	{
		Table:   "skills",
		Comment: []string{"Where SKILL.md files are discovered. Relative paths resolve against the workspace."},
		Keys: []Doc{
			{Key: "skills.dirs", Comment: "Roots to read skills from."},
			{Key: "skills.migrate_from", Comment: "Roots `rudy skills migrate` offers to copy from."},
		},
	},
	{
		Table:   "memory",
		Comment: []string{"The OKF memory bundle the memory plugin reads and folds."},
		Keys: []Doc{
			{Key: "memory.dir", Comment: "The bundle's directory."},
			{Key: "memory.enabled", Comment: "False loads the plugin and leaves it idle."},
			{Key: "memory.summary_model", Comment: "Model the fold summarises with; empty uses the session's.", Example: `"aperture:cline-pass/kimi-k3"`},
		},
	},
	{
		Table:   "mcp",
		Comment: []string{"MCP servers are configured in mcp.toml, which `rudy mcp add` writes."},
		Keys: []Doc{
			{Key: "mcp.connect_timeout_ms", Comment: "How long a server has to answer its handshake."},
		},
	},
	{
		Table: "ui",
		Comment: []string{
			"Every render choice is a field with a default, and the defaults below are the",
			"screen the design draws. Nothing renders that this table did not place.",
		},
		Keys: []Doc{
			{Key: "ui.render", Comment: "altscreen owns the terminal; inline draws in its scrollback and cannot re-expand a committed row.", Example: `"inline"`},
			{Key: "ui.vim", Comment: "Modal editing in the composer."},
			{Key: "ui.mouse", Comment: "off, the default, leaves the mouse to the terminal so a drag selects text to copy; click reports clicks and the wheel, which expands a tool row and scrolls; all reports movement too. With reporting on, selecting takes a modifier: Shift in Ghostty, WezTerm and most others, Option in Terminal.app and iTerm2.", Example: `"click"`},
			{Key: "ui.cats", Comment: "A cat face in the status line, one for the life of the client."},
		},
	},
	{
		Table:   "ui.layout",
		Comment: []string{"The slots drawn top to bottom. transcript, input and status are required once each."},
		Keys: []Doc{
			{Key: "ui.layout.slots", Comment: "header is optional and is where a plugin's header widget draws.", Example: `["header", "transcript", "input", "status"]`},
		},
	},
	{
		Table:   "ui.header",
		Comment: []string{"The header drawn once at the top of the transcript, which the conversation scrolls away."},
		Keys: []Doc{
			{Key: "ui.header.show", Comment: "False opens on an empty transcript."},
			{Key: "ui.header.animate", Comment: "The cat materialises once on startup. Altscreen only; any key skips it."},
			{Key: "ui.header.frame", Comment: "False draws the same lines with no box around them."},
			{Key: "ui.header.greeting", Comment: "The time of day and a name."},
			{Key: "ui.header.mark", Comment: "The cat."},
			{Key: "ui.header.name", Comment: "The greeting's name; empty reads git user.name, then the OS user.", Example: `"Guy"`},
			{Key: "ui.header.facts", Comment: "The session's own facts, in the order they draw: model, thinking, mode, workspace.", Example: `["model", "mode", "workspace"]`},
			{Key: "ui.header.tips", Comment: "Tips in the right column, rotated by the day. 0 draws none."},
			{Key: "ui.header.updates", Comment: "Release notes from the CHANGELOG the binary was built with. 0 draws none."},
			{Key: "ui.header.max_width", Comment: "The widest the box draws; a wider terminal leaves the rest of the line alone."},
		},
	},
	{
		Table:   "ui.input",
		Comment: []string{"The composer."},
		Keys: []Doc{
			{Key: "ui.input.rules", Comment: "The composer lines: one above carrying the session's name, one below carrying the context percentage."},
		},
	},
	{
		Table:   "ui.spinner",
		Comment: []string{"The glyph the turn cell animates while the model is working."},
		Keys: []Doc{
			{Key: "ui.spinner.name", Comment: "arc, blocks, pulse, paw or dots (the braille one every other CLI uses).", Example: `"dots"`},
			{Key: "ui.spinner.frames", Comment: "Your own frames, in order, each one cell wide. Empty uses the preset's.", Example: `["\u25f4", "\u25f7", "\u25f6", "\u25f5"]`},
			{Key: "ui.spinner.interval_ms", Comment: "How long each frame is on screen. 0 uses the preset's own timing.", Example: "150"},
		},
	},
	{
		Table:   "ui.transcript",
		Comment: []string{"Rows: what a tool call folds to, and what an assistant message looks like."},
		Keys: []Doc{
			{Key: "ui.transcript.tool_collapsed", Comment: "False opens every tool row."},
			{Key: "ui.transcript.tool_preview_lines", Comment: "Lines of a tool's result shown under a collapsed row."},
			{Key: "ui.transcript.thinking", Comment: "hidden or shown. ctrl+t toggles it for one run.", Example: `"shown"`},
			{Key: "ui.transcript.user_prefix", Comment: "The glyph a user message opens with."},
			{Key: "ui.transcript.block_gap", Comment: "Blank lines between assistant blocks."},
		},
	},
	{
		Table:   "ui.diff",
		Comment: []string{"How an edit's hunk is drawn."},
		Keys: []Doc{
			{Key: "ui.diff.style", Comment: "text is red and green text; background paints the line, the one painted background in the client.", Example: `"background"`},
		},
	},
	{
		Table:   "ui.status",
		Comment: []string{"The status line under the composer."},
		Keys: []Doc{
			{Key: "ui.status.above_editor", Comment: "The status line, above the composer line: what is worth seeing while you type rather than after.", Example: `["turn", "context"]`},
			{Key: "ui.status.items", Comment: "Built-ins are vim_mode, model, permission_mode, context, cost, workspace, turn, cat; a plugin's is \"<plugin>:<key>\".", Example: `["vim_mode", "model", "context", "cost", "workspace", "turn"]`},
			{Key: "ui.status.host", Comment: "Prefix the workspace item with host: when the session runs on a --host."},
		},
	},
	{
		Table:   "ui.notices",
		Comment: []string{"Notices are chrome under the transcript and never reach a session log."},
		Keys: []Doc{
			{Key: "ui.notices.max", Comment: "Lines drawn, newest last. 0 draws none."},
			{Key: "ui.notices.ttl_ms", Comment: "How long a notice stays before it goes. 0 keeps it until a newer one pushes it out.", Example: "0"},
		},
	},
	{
		Table: "ui.icons",
		Comment: []string{
			"The glyph set. nerd needs a patched font, unicode needs none, ascii is plain text.",
			"Any single icon can be replaced, and \"\" turns one off.",
		},
		Keys: []Doc{
			{Key: "ui.icons.set", Comment: "nerd, unicode or ascii.", Example: `"unicode"`},
		},
		Sample: []string{
			"branch = \"\"          # names: branch, model, context, tool, bash, edit, read,",
			"model  = \"\"          # write, grep, glob, fetch, agent, info, warn, error",
		},
	},
	{
		Table: "ui.theme",
		Comment: []string{
			"Colour roles. A value is a hex colour or another role's name; code takes a",
			"chroma style. Themes live in themes/<name>.toml under this directory.",
		},
		Keys: []Doc{
			{Key: "ui.theme.name", Comment: "A file under themes/ or the built-in.", Example: `"tokyonight"`},
		},
		Sample: []string{
			`accent = "#7aa2f7"`,
			`text   = "#c0caf5"`,
			`muted  = "#565f89"`,
			`code   = "chroma:tokyonight-night"`,
		},
	},
	{
		// No header of its own: every provider is its own table, and the sample below
		// carries the headers a reader copies.
		Comment: []string{
			"One table per endpoint. wire is anthropic_messages, openai_chat or custom;",
			"auth is env:NAME or cache:KEY, and is left out when the endpoint needs none.",
			"The models themselves are discovered from the endpoint, never listed here.",
		},
		Sample: []string{
			"[providers.aperture]",
			`wire = "openai_chat"`,
			`base_url = "https://ai.example.ts.net/v1"`,
			`dialect = "clinepass"`,
			"",
			"[providers.anthropic]",
			`wire = "anthropic_messages"`,
			`base_url = "https://api.anthropic.com"`,
			`auth = "env:ANTHROPIC_API_KEY"`,
		},
	},
	{
		Table: "keys",
		Comment: []string{
			"pi's action ids bound to pi's keys. A value replaces the default for that action",
			"and [] unbinds it. An id outside the closed set is an error naming it.",
		},
		Sample: []string{
			`"app.model.select" = "ctrl+l"`,
			`"app.tools.expand" = ["ctrl+o", "ctrl+e"]`,
		},
	},
	{
		Table:   "plugins",
		Comment: []string{"Plugins are linked in or spawned from a manifest; this table only turns them off and hands each its own settings."},
		Keys: []Doc{
			{Key: "plugins.disabled", Comment: "Names not to load.", Example: `["memory"]`},
		},
		Sample: []string{
			"[plugins.memory]",
			"# every key here is handed to the plugin verbatim",
		},
	},
}

// DocFor is the documentation for one key, and whether the catalogue has any.
func DocFor(key string) (Doc, bool) {
	for _, s := range Sections {
		for _, d := range s.Keys {
			if d.Key == key {
				return d, true
			}
		}
	}
	return Doc{}, false
}

// DocumentedKeys is every key the catalogue describes, in the order it writes them.
func DocumentedKeys() []string {
	var out []string
	for _, s := range Sections {
		for _, d := range s.Keys {
			out = append(out, d.Key)
		}
	}
	return out
}

// Example is the config file this repository ships and `rudy config example` prints: every
// key with its default, under the comment that says what it is for. It is generated rather
// than kept by hand, so a default that changes changes the example too.
func Example() string {
	var b strings.Builder
	defaults := Defaults()
	for i, s := range Sections {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, line := range s.Comment {
			writeComment(&b, line)
		}
		if s.Table != "" {
			fmt.Fprintf(&b, "[%s]\n", s.Table)
		} else if len(s.Comment) > 0 && len(s.Keys) > 0 {
			// The file's own opening paragraph, then a blank line: without a table header
			// under it, it would run straight into the first key's own comment.
			b.WriteString("\n")
		}
		for _, d := range s.Keys {
			writeComment(&b, d.Comment)
			if d.Example != "" {
				writeComment(&b, "e.g. "+d.Example)
			}
			fmt.Fprintf(&b, "%s = %s\n", leaf(d.Key), Literal(defaults[d.Key]))
		}
		for _, line := range s.Sample {
			if line == "" {
				b.WriteString("\n")
				continue
			}
			fmt.Fprintf(&b, "# %s\n", line)
		}
	}
	return b.String()
}

// writeComment writes one comment line, or a bare hash for an empty one.
func writeComment(b *strings.Builder, line string) {
	if line == "" {
		b.WriteString("#\n")
		return
	}
	fmt.Fprintf(b, "# %s\n", line)
}

// leaf is the part of a dotted key that is written under its table.
func leaf(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

// Literal is a default value as TOML writes it. The types are the ones Defaults carries:
// strings, bools, ints, floats and string slices.
func Literal(v any) string {
	switch t := v.(type) {
	case nil:
		return `""`
	case string:
		return quote(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", t)
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", t), "0"), ".")
	case []string:
		parts := make([]string, 0, len(t))
		for _, s := range t {
			parts = append(parts, quote(s))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprintf("%v", v)
}

// quote is a TOML basic string.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
