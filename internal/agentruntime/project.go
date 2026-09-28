// SPDX-License-Identifier: AGPL-3.0-or-later

package agentruntime

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/guygrigsby/rudy/internal/session"
)

const projectionDomain = "rudy.runtime.projection.v1"

// Project converts one canonical runtime item into the derived client view. Identity uses
// only the complete runtime binding, so a cold thread read and the corresponding live item
// completion produce the same row id.
func Project(thread Thread, turn Turn, item Item) ProjectedEntry {
	return ProjectedEntry{
		ID: projectionID(thread.Runtime, thread.ThreadID, turn.TurnID, item.ItemID, string(item.Type)),
		At: time.Now().UTC(), Kind: item.Type, Runtime: thread.Runtime, ThreadID: thread.ThreadID,
		TurnID: turn.TurnID, ItemID: item.ItemID, Content: projectContent(item), Status: item.Status, Usage: turn.Usage,
	}
}

func projectionID(parts ...string) ulid.ULID {
	h := sha256.New()
	writeProjectionPart(h, projectionDomain)
	for _, part := range parts {
		writeProjectionPart(h, part)
	}
	sum := h.Sum(nil)
	var id ulid.ULID
	copy(id[:], sum[:len(id)])
	return id
}

type projectionWriter interface {
	Write([]byte) (int, error)
}

func writeProjectionPart(w projectionWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = w.Write(size[:])
	_, _ = w.Write([]byte(value))
}

func projectContent(item Item) []session.Block {
	if len(item.Content) > 0 {
		return append([]session.Block(nil), item.Content...)
	}
	var lines []string
	if item.Command != "" {
		lines = append(lines, item.Command)
	}
	if item.CWD != "" {
		lines = append(lines, item.CWD)
	}
	if item.Output != "" {
		lines = append(lines, item.Output)
	}
	for _, change := range item.Changes {
		lines = append(lines, strings.TrimSpace(change.Kind+" "+change.Path))
	}
	if item.Error != "" {
		lines = append(lines, item.Error)
	}
	if len(lines) == 0 {
		return nil
	}
	return []session.Block{session.TextBlock(strings.Join(lines, "\n"))}
}
