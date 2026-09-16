package strictjson

import (
	"fmt"
	"strings"
	"testing"
)

func TestObjectAndCombinedUnitBounds(t *testing.T) {
	for _, count := range []int{4096, 4097} {
		var raw strings.Builder
		raw.WriteByte('{')
		for i := range count {
			if i > 0 {
				raw.WriteByte(',')
			}
			fmt.Fprintf(&raw, "\"k%d\":0", i)
		}
		raw.WriteByte('}')
		if (Scan([]byte(raw.String()), Outer) == nil) != (count == 4096) {
			t.Fatal("wrong object limit")
		}
	}
	for _, count := range []int{16382, 16384} {
		arr := "[" + strings.Repeat("0,", count-1) + "0]"
		raw := "[" + arr + "," + arr + "," + arr + "," + arr + "]"
		if (Scan([]byte(raw), Outer) == nil) != (count == 16382) {
			t.Fatal("wrong combined structural limit")
		}
	}
}

func TestProfiles(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		ok        bool
	}{
		{"valid", `{"a":[true,null,1.25,"hi"]}`, true},
		{"escaped duplicate", `{"a":1,"\u0061":2}`, false},
		{"nested duplicate", `{"x":{"sessionId":1,"sessionId":2}}`, false},
		{"request duplicate", `{"requestId":1,"requestId":2}`, false},
		{"utf8", "\"\xff\"", false},
		{"surrogate", `"\ud800"`, false},
		{"pair", `"\ud800\udc00"`, true},
		{"suffix", `{} {}`, false},
		{"depth limit", strings.Repeat("[", 63) + "0" + strings.Repeat("]", 63), true},
		{"depth overflow", strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64), false},
		{"array limit", "[" + strings.Repeat("0,", 16383) + "0]", true},
		{"array overflow", "[" + strings.Repeat("0,", 16384) + "0]", false},
		{"million nodes", "[" + strings.Repeat("0,", 1000000) + "0]", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := Scan([]byte(tt.raw), Outer); (err == nil) != tt.ok {
				t.Fatalf("accept=%v, want %v", err == nil, tt.ok)
			}
		})
	}
	if err := Scan([]byte(strings.Repeat("[", 71)+"0"+strings.Repeat("]", 71)), Event); err != nil {
		t.Fatal(err)
	}
	if err := Scan([]byte(strings.Repeat("[", 72)+"0"+strings.Repeat("]", 72)), Event); err == nil {
		t.Fatal("accepted depth 73")
	}
	if err := Scan([]byte(`{"a":[0,1]}`), Profile{Depth: 64, Units: 5, Array: 16384, Object: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := Scan([]byte(`{"a":[0,1]}`), Profile{Depth: 64, Units: 4, Array: 16384, Object: 4096}); err == nil {
		t.Fatal("accepted excess units")
	}
}
