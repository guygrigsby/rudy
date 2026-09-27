// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

const maxWireBytes = 16 << 20

type callResult struct {
	value json.RawMessage
	err   error
}

type peer struct {
	reader io.Reader
	writer io.Writer
	closer io.Closer

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan callResult
	done    chan struct{}
	err     error

	onRequest      inboundHandler
	onNotification notificationHandler
}

func newPeer(reader io.Reader, writer io.Writer, closer io.Closer, onRequest inboundHandler, onNotification notificationHandler) *peer {
	p := &peer{
		reader: reader, writer: writer, closer: closer,
		pending: make(map[string]chan callResult), done: make(chan struct{}),
		onRequest: onRequest, onNotification: onNotification,
	}
	go p.readLoop()
	return p
}

func (p *peer) Call(ctx context.Context, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("codex app server %s params: %w", method, err)
	}

	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return err
	}
	p.nextID++
	id := strconv.FormatUint(p.nextID, 10)
	wait := make(chan callResult, 1)
	p.pending[id] = wait
	p.mu.Unlock()

	message := envelope{ID: json.RawMessage(id), Method: method, Params: rawParams}
	if err := p.write(message); err != nil {
		p.removePending(id)
		p.fail(err)
		return err
	}

	select {
	case got := <-wait:
		if got.err != nil {
			return got.err
		}
		if result == nil || len(got.value) == 0 || string(got.value) == "null" {
			return nil
		}
		if err := json.Unmarshal(got.value, result); err != nil {
			return fmt.Errorf("codex app server %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		p.removePending(id)
		return ctx.Err()
	}
}

func (p *peer) Notify(method string, params any) error {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("codex app server %s params: %w", method, err)
	}
	return p.write(envelope{Method: method, Params: rawParams})
}

func (p *peer) write(message envelope) error {
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(b) > maxWireBytes {
		return fmt.Errorf("codex app server message exceeds %d bytes", maxWireBytes)
	}
	b = append(b, '\n')
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.writer.Write(b); err != nil {
		return fmt.Errorf("codex app server write: %w", err)
	}
	return nil
}

func (p *peer) readLoop() {
	scanner := bufio.NewScanner(p.reader)
	scanner.Buffer(make([]byte, 64<<10), maxWireBytes)
	for scanner.Scan() {
		var message envelope
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			p.fail(fmt.Errorf("codex app server decode: %w", err))
			return
		}
		if message.Method != "" {
			if len(message.ID) == 0 {
				if p.onNotification != nil {
					p.onNotification(message.Method, message.Params)
				}
				continue
			}
			go p.handleRequest(message)
			continue
		}
		if len(message.ID) == 0 {
			p.fail(errors.New("codex app server message has neither method nor id"))
			return
		}
		p.resolve(message)
	}
	if err := scanner.Err(); err != nil {
		p.fail(fmt.Errorf("codex app server read: %w", err))
		return
	}
	p.fail(io.EOF)
}

func (p *peer) handleRequest(message envelope) {
	var (
		result any
		err    error
	)
	if p.onRequest == nil {
		err = &wireError{Code: -32601, Message: "method not supported"}
	} else {
		result, err = p.onRequest(message.Method, message.Params)
	}
	response := envelope{ID: message.ID}
	if err != nil {
		var appErr *wireError
		if errors.As(err, &appErr) {
			response.Error = appErr
		} else {
			response.Error = &wireError{Code: -32603, Message: err.Error()}
		}
	} else {
		response.Result, err = json.Marshal(result)
		if err != nil {
			response.Error = &wireError{Code: -32603, Message: "could not encode response"}
		}
	}
	if err := p.write(response); err != nil {
		p.fail(err)
	}
}

func (p *peer) resolve(message envelope) {
	id := string(message.ID)
	p.mu.Lock()
	wait := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()
	if wait == nil {
		return
	}
	if message.Error != nil {
		wait <- callResult{err: message.Error}
		return
	}
	wait <- callResult{value: message.Result}
}

func (p *peer) removePending(id string) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

func (p *peer) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return
	}
	p.err = err
	waits := p.pending
	p.pending = make(map[string]chan callResult)
	close(p.done)
	p.mu.Unlock()
	for _, wait := range waits {
		wait <- callResult{err: err}
	}
}

func (p *peer) Close() error {
	err := p.closer.Close()
	p.fail(errors.New("codex app server closed"))
	return err
}

func (p *peer) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
