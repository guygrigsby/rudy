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
	"github.com/guygrigsby/rudy/internal/session"
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

// decode unpacks a tool's arguments and resolves the session's project in one step, since
// every memory tool needs both and neither is worth doing without the other. The second
// result is the failure to answer with when it is not empty.
func decode[T any](p *memPlugin, call tool.Call, a *T) (project string, model session.ModelRef, fail *tool.Result) {
	if err := json.Unmarshal(call.Input, a); err != nil {
		res := fsroot.Fail("%s: bad input: %v", call.Name, err)
		return "", model, &res
	}
	project, model = p.session(call.SessionID.String())
	if project == "" {
		res := fsroot.Fail("memory is off for this session")
		return "", model, &res
	}
	return project, model, nil
}

func (p *memPlugin) remember(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a rememberArgs
	project, model, fail := decode(p, call, &a)
	if fail != nil {
		return *fail, nil
	}
	target := project
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
	res, err := memory.Remember(p.b, dir, actorFor(model), time.Now(), in)
	if err != nil {
		return fsroot.Fail("memory_remember: %v", err), nil
	}
	verb := "revised"
	if res.Created {
		verb = "remembered"
	}
	// The job regenerates the index and commits. It runs here rather than in a goroutine so
	// the model's next recall sees the index this write produced.
	text := verb + " " + res.Rel
	if _, err := res.Job.Run(p.b); err != nil {
		text += "; index and commit failed: " + err.Error()
	}
	return fsroot.Text(text), nil
}

func (p *memPlugin) recall(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a recallArgs
	project, _, fail := decode(p, call, &a)
	if fail != nil {
		return *fail, nil
	}
	hits, err := memory.Recall(p.b, project, a.Type, a.Query, a.Deprecated)
	if err != nil {
		return fsroot.Fail("memory_recall: %v", err), nil
	}
	return jsonResult("memory_recall", hits)
}

func (p *memPlugin) recallObservation(ctx context.Context, call tool.Call) (tool.Result, error) {
	var a observationArgs
	project, _, fail := decode(p, call, &a)
	if fail != nil {
		return *fail, nil
	}
	// The SDK refuses to follow a transcript path outside the user's home; it is the one
	// argument it cannot work out for itself.
	home, err := os.UserHomeDir()
	if err != nil {
		return fsroot.Fail("memory_recall_observation: %v", err), nil
	}
	hit, err := memory.RecallObservation(p.b, project, a.ID, home)
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
