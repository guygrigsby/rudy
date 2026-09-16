// Package acpwire owns bounded raw ACP framing, outside SDK types.
package acpwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"github.com/guygrigsby/rudy/internal/strictjson"
)

const MaxIDBytes = 4096
const MaxOutboundID int64 = 9007199254740991

var ErrID = errors.New("invalid ACP id")

type IDKind uint8

const (
	NullID IDKind = iota
	StringID
	NumberID
)

// ID is a canonical key. Inbound and outbound ledgers are deliberately separate.
type ID struct {
	Kind  IDKind
	Value string
}

func ParseID(raw []byte) (ID, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || strictjson.Scan(raw, strictjson.Outer) != nil {
		return ID{}, ErrID
	}
	if bytes.Equal(raw, []byte("null")) {
		return ID{Kind: NullID}, nil
	}
	if raw[0] == '"' {
		var v string
		if json.Unmarshal(raw, &v) != nil || len(v) > MaxIDBytes {
			return ID{}, ErrID
		}
		return ID{StringID, v}, nil
	}
	if len(raw) > MaxIDBytes {
		return ID{}, ErrID
	}
	token := string(raw)
	coefficient := token
	exp := 0
	if i := strings.IndexAny(token, "eE"); i >= 0 {
		coefficient = token[:i]
		e := token[i+1:]
		negative := false
		if len(e) > 0 && (e[0] == '+' || e[0] == '-') {
			negative = e[0] == '-'
			e = e[1:]
		}
		for _, c := range e {
			if c < '0' || c > '9' {
				return ID{}, ErrID
			}
			exp = exp*10 + int(c-'0')
			if exp > 4096 {
				return ID{}, ErrID
			}
		}
		if negative {
			exp = -exp
		}
	}
	digits := 0
	for _, c := range coefficient {
		if c >= '0' && c <= '9' {
			digits++
		} else if c != '-' && c != '.' {
			return ID{}, ErrID
		}
	}
	if digits == 0 || digits > 4096 {
		return ID{}, ErrID
	}
	if i := strings.IndexByte(coefficient, '.'); i >= 0 {
		exp -= len(coefficient) - i - 1
		coefficient = coefficient[:i] + coefficient[i+1:]
	}
	n, ok := new(big.Int).SetString(coefficient, 10)
	if !ok {
		return ID{}, ErrID
	}
	if n.Sign() != 0 && exp != 0 {
		magnitude := exp
		if magnitude < 0 {
			magnitude = -magnitude
		}
		power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(magnitude)), nil)
		if exp > 0 {
			n.Mul(n, power)
		} else {
			remainder := new(big.Int)
			n.QuoRem(n, power, remainder)
			if remainder.Sign() != 0 {
				return ID{}, ErrID
			}
		}
	}
	if !n.IsInt64() {
		return ID{}, ErrID
	}
	return ID{NumberID, n.String()}, nil
}
func (id ID) Raw() json.RawMessage {
	switch id.Kind {
	case NullID:
		return json.RawMessage("null")
	case NumberID:
		return json.RawMessage(id.Value)
	default:
		b, _ := json.Marshal(id.Value)
		return b
	}
}
func numericID(n int64) ID { return ID{NumberID, strconv.FormatInt(n, 10)} }
