// Package acpschema pins and validates the stable ACP v1 schema shipped by the
// selected ACP SDK. The maintained github.com/google/jsonschema-go v0.4.3
// validator is selected under its MIT license. Validation is structural.
// Callers must enforce numeric format widths separately because that validator
// intentionally ignores format.
package acpschema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	Version = "0.13.5"
	SHA256  = "0da9fe718746ccc2e8ef789efa6687e64a252dea3a8789faae9a1acbe60e0d3b"
)

//go:embed schema.json
var schemaBytes []byte

var (
	resolveOnce sync.Once
	rootSchema  *jsonschema.Schema
	resolved    *jsonschema.Resolved
	resolveErr  error
)

// Bytes returns a copy of the pinned stable schema.
func Bytes() []byte {
	return bytes.Clone(schemaBytes)
}

// Validate checks one complete ACP envelope against the pinned stable schema.
func Validate(raw []byte) error {
	value, err := decodeJSON(raw)
	if err != nil {
		return err
	}
	validator, err := rootValidator()
	if err != nil {
		return err
	}
	return validator.Validate(value)
}

// ValidateDefinition checks a value against one named definition in the pinned schema.
func ValidateDefinition(name string, raw []byte) error {
	value, err := decodeJSON(raw)
	if err != nil {
		return err
	}
	if _, err := rootValidator(); err != nil {
		return err
	}
	if _, ok := rootSchema.Defs[name]; !ok {
		return fmt.Errorf("ACP schema definition %q not found", name)
	}
	definition := &jsonschema.Schema{
		Schema: rootSchema.Schema,
		Defs:   rootSchema.Defs,
		Ref:    "#/$defs/" + name,
	}
	validator, err := definition.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolve ACP schema definition %q: %w", name, err)
	}
	return validator.Validate(value)
}

func rootValidator() (*jsonschema.Resolved, error) {
	resolveOnce.Do(func() {
		rootSchema = new(jsonschema.Schema)
		if err := json.Unmarshal(schemaBytes, rootSchema); err != nil {
			resolveErr = fmt.Errorf("decode pinned ACP schema: %w", err)
			return
		}
		resolved, resolveErr = rootSchema.Resolve(nil)
		if resolveErr != nil {
			resolveErr = fmt.Errorf("resolve pinned ACP schema: %w", resolveErr)
		}
	})
	return resolved, resolveErr
}

func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode ACP JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode ACP JSON: multiple values")
		}
		return nil, fmt.Errorf("decode ACP JSON suffix: %w", err)
	}
	return value, nil
}
