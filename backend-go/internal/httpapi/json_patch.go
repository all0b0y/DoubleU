package httpapi

import (
	"bytes"
	"encoding/json"
)

type optionalBoolPatch struct {
	Present bool
	Value   *bool
}

func (p *optionalBoolPatch) UnmarshalJSON(data []byte) error {
	p.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		p.Value = nil
		return nil
	}
	var value bool
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	p.Value = &value
	return nil
}

func (p optionalBoolPatch) Apply(current *bool) *bool {
	if !p.Present {
		return cloneBoolPtr(current)
	}
	return cloneBoolPtr(p.Value)
}

type optionalInt64Patch struct {
	Present bool
	Value   int64
}

func (p *optionalInt64Patch) UnmarshalJSON(data []byte) error {
	p.Present = true
	return json.Unmarshal(data, &p.Value)
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

// boolOrDefault resolves an optional JSON bool to a concrete value: the pointer
// when present, else fallback. Used for non-nullable bool columns where an
// omitted field must keep a default rather than collapse to false.
func boolOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

// stringOrDefault resolves an optional JSON string to a concrete value: the
// dereferenced pointer when non-nil and non-empty, else fallback. Used for
// non-nullable string columns (e.g. model_tier) with a known default.
func stringOrDefault(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}
