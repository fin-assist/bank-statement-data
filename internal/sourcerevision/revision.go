// Package sourcerevision validates private provenance without transport or storage dependencies.
package sourcerevision

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
)

var code = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var ErrInvalid = errors.New("invalid source revisions")

// Valid permits bounded open kinds and exact nonnegative counters, including known zero.
func Valid(values map[string]int64) bool {
	if len(values) > 32 {
		return false
	}
	for key, value := range values {
		if !code.MatchString(key) || value < 0 {
			return false
		}
	}
	return true
}

// Decode retains unknown legacy metadata but never interprets null as known zero.
func Decode(data []byte) (map[string]int64, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) > 32 {
		return nil, ErrInvalid
	}
	values := make(map[string]int64, len(fields))
	for key, raw := range fields {
		var value int64
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return nil, ErrInvalid
		}
		values[key] = value
	}
	if !Valid(values) {
		return nil, ErrInvalid
	}
	return values, nil
}
