package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	memory "github.com/aeryx-ai/memory/memory-go"

	"github.com/guygrigsby/rudy/internal/plugin"
	"github.com/guygrigsby/rudy/internal/plugins/tools/fsroot"
	"github.com/guygrigsby/rudy/internal/tool"
)

// The three tools are safe: they never touch the workspace, and every bundle write goes
// through the SDK, which refuses anything that would break an OKF invariant. There is no
// input a model can send that a permission prompt would have caught.

// rememberSchema names the SDK's own type vocabulary in its enum, so the schema the model
// sees cannot drift from what Remember accepts. Init owns the failure: a plugin whose schema
// will not build has nothing to register.
func rememberSchema() (json.RawMessage, error) {
	types, err := json.Marshal(memory.Types)
	if err != nil {
		return nil, fmt.Errorf("memory_remember schema: %w", err)
	}
	return json.RawMessage(`{"type":"object","properties":{` +
		`"type":{"type":"string","enum":` + string(types) + `,"description":"Concept type"},` +
		`"title":{"type":"string","description":"Short title; its slug is the file name, so writing the same title again revises that concept"},` +
		`"description":{"type":"string","description":"One line summarizing the concept"},` +
		`"body":{"type":"string","description":"Markdown body"},` +
		`"tags":{"type":"array","items":{"type":"string"},"description":"Tags for recall"},` +
		`"scope":{"type":"string","enum":["project","root"],"description":"project (default) writes under this workspace's project; root writes bundle-wide"}` +
		`},"required":["type","title"],"additionalProperties":false}`), nil
}

const recallSchema = `{"type":"object","properties":{` +
	`"query":{"type":"string","description":"Terms matched against title, tags, description and body; empty returns everything"},` +
	`"type":{"type":"string","description":"Restrict to one concept type"},` +
	`"deprecated":{"type":"boolean","description":"Include deprecated concepts"}` +
	`},"required":["query"],"additionalProperties":false}`

const recallObservationSchema = `{"type":"object","properties":{` +
	`"id":{"type":"string","description":"Observation or reflection id from a session summary"}` +
	`},"required":["id"],"additionalProperties":false}`

type rememberArgs struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description *string  `json:"description"`
	Body        *string  `json:"body"`
	Tags        []string `json:"tags"`
	Scope       string   `json:"scope"`
}

type recallArgs struct {
	Query      string `json:"query"`
	Type       string `json:"type"`
	Deprecated bool   `json:"deprecated"`
}

type observationArgs struct {
	ID string `json:"id"`
}

func (p *memPlugin) registerTools(h plugin.Host) error {
	remember, err := rememberSchema()
	if err != nil {
		return err
	}
	for _, t := range []tool.Tool{
		{
			Name:        "memory_remember",
			Description: "Record a durable fact, preference or reference in the memory bundle. Writing the same title again revises the existing concept rather than adding a second one.",
			Schema:      remember,
			Safety:      tool.Safe,
			Invoke:      p.remember,
		},
		{
			Name:        "memory_recall",
			Description: "Search the memory bundle for concepts matching a query, over the bundle root and this workspace's project.",
			Schema:      json.RawMessage(recallSchema),
			Safety:      tool.Safe,
			Invoke:      p.recall,
		},
		{
			Name:        "memory_recall_observation",
			Description: "Show one session summary observation or reflection with the transcript entries it was drawn from.",
			Schema:      json.RawMessage(recallObservationSchema),
			Safety:      tool.Safe,
			Invoke:      p.recallObservation,
		},
	} {
		if err := h.RegisterTool(t); err != nil {
			return err
		}
	}
	return nil
}

// scopeOf resolves the session a tool call came from. A session that never opened, or one
// whose workspace had no project id, has nowhere to read or write, and saying so is a better
// answer than guessing at the bundle root. A child session is not special here: a subagent
// reads and records into its parent's project like any other caller.
func (p *memPlugin) scopeOf(call tool.Call) (sessionState, *tool.Result) {
	st, ok := p.session(call.SessionID.String())
	if !ok || st.project == "" {
		res := fsroot.Fail("memory is off for this session")
		return st, &res
	}
	return st, nil
}

func (p *memPlugin) remember(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a rememberArgs
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("memory_remember: bad input: %v", err), nil
	}
	st, fail := p.scopeOf(call)
	if fail != nil {
		return *fail, nil
	}
	target := st.project
	if a.Scope == "root" {
		target = ""
	}
	dir, err := p.b.Dir(target)
	if err != nil {
		return fsroot.Fail("memory_remember: %v", err), nil
	}
	in := memory.RememberInput{Type: a.Type, Title: a.Title, Description: a.Description, Body: a.Body}
	if a.Tags != nil {
		in.Tags = &a.Tags
	}
	res, err := memory.Remember(p.b, dir, actorFor(st.model), time.Now(), in)
	if err != nil {
		return fsroot.Fail("memory_remember: %v", err), nil
	}
	// The concept is on disk; the model is told so now. Regenerating the index, committing
	// and pushing is a separate job with a git remote at the end of it, and a safe tool does
	// not hold a turn open on network egress. A failure there becomes a note.
	p.runJob(call.SessionID.String(), res.Job)
	verb := "revised"
	if res.Created {
		verb = "remembered"
	}
	return fsroot.Text(verb + " " + res.Rel), nil
}

func (p *memPlugin) recall(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a recallArgs
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("memory_recall: bad input: %v", err), nil
	}
	st, fail := p.scopeOf(call)
	if fail != nil {
		return *fail, nil
	}
	hits, err := memory.Recall(p.b, st.project, a.Type, a.Query, a.Deprecated)
	if err != nil {
		return fsroot.Fail("memory_recall: %v", err), nil
	}
	return jsonResult("memory_recall", hits)
}

func (p *memPlugin) recallObservation(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a observationArgs
	if err := json.Unmarshal(call.Input, &a); err != nil {
		return fsroot.Fail("memory_recall_observation: bad input: %v", err), nil
	}
	st, fail := p.scopeOf(call)
	if fail != nil {
		return *fail, nil
	}
	// The SDK refuses to follow a transcript path outside the user's home; it is the one
	// argument it cannot work out for itself.
	home, err := os.UserHomeDir()
	if err != nil {
		return fsroot.Fail("memory_recall_observation: %v", err), nil
	}
	hit, err := memory.RecallObservation(p.b, st.project, a.ID, home)
	if err != nil {
		return fsroot.Fail("memory_recall_observation: %v", err), nil
	}
	return jsonResult("memory_recall_observation", hit)
}

// jsonResult renders a recall answer. Both recalls answer structured data rather than prose,
// so the model can cite an id back without the plugin inventing a text format.
func jsonResult(name string, v any) (tool.Result, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return fsroot.Fail("%s: %v", name, err), nil
	}
	return fsroot.Text(string(raw)), nil
}
