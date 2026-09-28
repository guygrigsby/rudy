// SPDX-License-Identifier: AGPL-3.0-or-later

package agentruntime_test

import (
	"testing"

	"github.com/guygrigsby/rudy/internal/agentruntime"
	"github.com/guygrigsby/rudy/internal/session"
)

func TestProjectionIDIsStableAcrossColdReadAndLiveCompletion(t *testing.T) {
	thread := agentruntime.Thread{Runtime: "codex", ThreadID: "thread-1"}
	turn := agentruntime.Turn{TurnID: "turn-1", Status: agentruntime.TurnCompleted, Usage: session.Usage{Input: 7, Output: 3}}
	item := agentruntime.Item{
		ItemID: "item-1", Type: agentruntime.ItemAgentMessage, Status: "completed",
		Content: []session.Block{session.TextBlock("answer")},
	}
	cold := agentruntime.Project(thread, turn, item)
	live := agentruntime.Project(thread, turn, item)
	if cold.ID != live.ID {
		t.Fatalf("projection ids differ: %s != %s", cold.ID, live.ID)
	}
	if cold.ID.IsZero() || cold.Runtime != "codex" || cold.ThreadID != "thread-1" || cold.TurnID != "turn-1" || cold.ItemID != "item-1" {
		t.Fatalf("projection binding = %+v", cold)
	}
	if cold.Kind != agentruntime.ItemAgentMessage || cold.Status != "completed" || cold.Usage != turn.Usage {
		t.Fatalf("projection content = %+v", cold)
	}
}

func TestProjectionIDChangesWithEveryBindingPart(t *testing.T) {
	baseThread := agentruntime.Thread{Runtime: "codex", ThreadID: "thread-1"}
	baseTurn := agentruntime.Turn{TurnID: "turn-1"}
	baseItem := agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemPlan}
	base := agentruntime.Project(baseThread, baseTurn, baseItem).ID
	cases := []agentruntime.ProjectedEntry{
		agentruntime.Project(agentruntime.Thread{Runtime: "other", ThreadID: "thread-1"}, baseTurn, baseItem),
		agentruntime.Project(agentruntime.Thread{Runtime: "codex", ThreadID: "thread-2"}, baseTurn, baseItem),
		agentruntime.Project(baseThread, agentruntime.Turn{TurnID: "turn-2"}, baseItem),
		agentruntime.Project(baseThread, baseTurn, agentruntime.Item{ItemID: "item-2", Type: agentruntime.ItemPlan}),
		agentruntime.Project(baseThread, baseTurn, agentruntime.Item{ItemID: "item-1", Type: agentruntime.ItemDiff}),
	}
	for i, projected := range cases {
		if projected.ID == base {
			t.Fatalf("case %d did not affect projection id", i)
		}
	}
}
