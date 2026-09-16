// Package strictjson validates bounded JSON before allocating decoded graphs.
package strictjson

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// Profile bounds nesting (root is one), values plus member names and containers.
type Profile struct{ Depth, Units, Array, Object int }

var (
	Outer       = Profile{64, 65536, 16384, 4096}
	Event       = Profile{72, 131072, 16384, 4096}
	NestedInput = Profile{64, 65536, 16384, 4096}
	ErrInvalid  = errors.New("invalid JSON")
	ErrLimit    = errors.New("JSON structural limit")
)

// Scan retains only bounded nesting and decoded names in active objects.
// Errors deliberately contain no input data.
func Scan(raw []byte, p Profile) error {
	if p.Depth < 1 || p.Units < 1 || p.Array < 1 || p.Object < 1 {
		return ErrLimit
	}
	if !utf8.Valid(raw) {
		return ErrInvalid
	}
	s := scanner{raw: raw, p: p}
	if err := s.value(1); err != nil {
		return err
	}
	s.space()
	if s.i != len(raw) {
		return ErrInvalid
	}
	return nil
}

type scanner struct {
	raw      []byte
	i, units int
	p        Profile
}

func (s *scanner) space() {
	for s.i < len(s.raw) {
		switch s.raw[s.i] {
		case ' ', '\t', '\r', '\n':
			s.i++
		default:
			return
		}
	}
}
func (s *scanner) unit() error {
	s.units++
	if s.units > s.p.Units {
		return ErrLimit
	}
	return nil
}
func (s *scanner) value(depth int) error {
	if depth > s.p.Depth {
		return ErrLimit
	}
	if err := s.unit(); err != nil {
		return err
	}
	s.space()
	if s.i == len(s.raw) {
		return ErrInvalid
	}
	switch s.raw[s.i] {
	case '{', '[':
		object := s.raw[s.i] == '{'
		end := byte(']')
		limit := s.p.Array
		if object {
			end = '}'
			limit = s.p.Object
		}
		s.i++
		s.space()
		if s.take(end) {
			return nil
		}
		var keys map[string]struct{}
		if object {
			keys = make(map[string]struct{})
		}
		count := 0
		for {
			count++
			if count > limit {
				return ErrLimit
			}
			if object {
				if err := s.unit(); err != nil {
					return err
				}
				start := s.i
				if err := s.str(); err != nil {
					return err
				}
				var key string
				if json.Unmarshal(s.raw[start:s.i], &key) != nil {
					return ErrInvalid
				}
				if _, exists := keys[key]; exists {
					return ErrInvalid
				}
				keys[key] = struct{}{}
				s.space()
				if !s.take(':') {
					return ErrInvalid
				}
			}
			if err := s.value(depth + 1); err != nil {
				return err
			}
			s.space()
			if s.take(end) {
				return nil
			}
			if !s.take(',') {
				return ErrInvalid
			}
			s.space()
		}
	case '"':
		return s.str()
	case 't':
		return s.literal("true")
	case 'f':
		return s.literal("false")
	case 'n':
		return s.literal("null")
	default:
		return s.number()
	}
}
func (s *scanner) take(c byte) bool {
	if s.i < len(s.raw) && s.raw[s.i] == c {
		s.i++
		return true
	}
	return false
}
func (s *scanner) literal(v string) error {
	if len(s.raw)-s.i < len(v) || string(s.raw[s.i:s.i+len(v)]) != v {
		return ErrInvalid
	}
	s.i += len(v)
	return nil
}
func (s *scanner) hex() (uint16, error) {
	var v uint16
	for range 4 {
		if s.i == len(s.raw) {
			return 0, ErrInvalid
		}
		c := s.raw[s.i]
		s.i++
		v *= 16
		switch {
		case c >= '0' && c <= '9':
			v += uint16(c - '0')
		case c >= 'a' && c <= 'f':
			v += uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v += uint16(c - 'A' + 10)
		default:
			return 0, ErrInvalid
		}
	}
	return v, nil
}
func (s *scanner) str() error {
	if !s.take('"') {
		return ErrInvalid
	}
	for s.i < len(s.raw) {
		c := s.raw[s.i]
		s.i++
		if c == '"' {
			return nil
		}
		if c < 32 {
			return ErrInvalid
		}
		if c != '\\' {
			continue
		}
		if s.i == len(s.raw) {
			return ErrInvalid
		}
		c = s.raw[s.i]
		s.i++
		switch c {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			v, err := s.hex()
			if err != nil {
				return err
			}
			if v >= 0xdc00 && v <= 0xdfff {
				return ErrInvalid
			}
			if v >= 0xd800 && v <= 0xdbff {
				if !s.take('\\') || !s.take('u') {
					return ErrInvalid
				}
				w, err := s.hex()
				if err != nil || w < 0xdc00 || w > 0xdfff {
					return ErrInvalid
				}
			}
		default:
			return ErrInvalid
		}
	}
	return ErrInvalid
}
func (s *scanner) digits() int {
	start := s.i
	for s.i < len(s.raw) && s.raw[s.i] >= '0' && s.raw[s.i] <= '9' {
		s.i++
	}
	return s.i - start
}
func (s *scanner) number() error {
	s.take('-')
	if !s.take('0') {
		if s.digits() == 0 {
			return ErrInvalid
		}
	}
	if s.take('.') && s.digits() == 0 {
		return ErrInvalid
	}
	if s.take('e') || s.take('E') {
		if !s.take('+') {
			s.take('-')
		}
		if s.digits() == 0 {
			return ErrInvalid
		}
	}
	return nil
}
