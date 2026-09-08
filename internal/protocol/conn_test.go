package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestPipeRoundTrip(t *testing.T) {
	a, b := Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		if err := a.Send(ctx, map[string]string{"hello": "b"}); err != nil {
			t.Error(err)
		}
	}()
	got, err := b.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"hello":"b"}` {
		t.Fatalf("b got %s", got)
	}

	go func() {
		if err := b.Send(ctx, json.RawMessage(`{"hello":"a"}`)); err != nil {
			t.Error(err)
		}
	}()
	got, err = a.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"hello":"a"}` {
		t.Fatalf("a got %s", got)
	}
}

func TestPipeCloseGivesPeerEOF(t *testing.T) {
	a, b := Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("peer Recv after Close: want io.EOF, got %v", err)
	}
	if err := b.Send(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Send to a closed peer must fail")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
}

func TestPipeRecvHonorsContext(t *testing.T) {
	a, _ := Pipe()
	defer func() { _ = a.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.Recv(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
}
