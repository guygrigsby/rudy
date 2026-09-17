// SPDX-License-Identifier: AGPL-3.0-or-later

package httpx

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

// IdleBody wraps a streaming response body with an idle timeout: when d passes with no bytes
// read, cancel runs, which makes the blocked Read return and the request end. Close stops the
// timer and closes the underlying body. Every codec that streams needs this, and both need to
// classify the timeout the same way, so it lives here rather than in either of them.
//
// The cancel a caller hands over belongs to the request's own context, so the caller keeps its
// own defer cancel: a body that is never closed still releases when the turn's context does.
func IdleBody(body io.ReadCloser, d time.Duration, cancel context.CancelFunc) io.ReadCloser {
	b := &idleBody{rc: body, d: d}
	b.timer = time.AfterFunc(d, func() {
		b.fired.Store(true)
		cancel()
	})
	return b
}

// IdleFired reports whether the idle timeout fired on a body IdleBody wrapped. Anything else
// reports false, so a caller can ask about whatever reader it is holding.
func IdleFired(body io.Reader) bool {
	b, ok := body.(*idleBody)
	return ok && b.fired.Load()
}

type idleBody struct {
	rc    io.ReadCloser
	d     time.Duration
	timer *time.Timer
	fired atomic.Bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.timer.Reset(b.d)
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}
