package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/invopop/jsonschema"
)

// Duration is a config time window that accepts a bare number of seconds
// (the legacy form), the string "off" for zero, or a Go duration string
// such as "5m" or "1h30m". One rule for the knob it configures: 0 or
// "off" disables it, and a negative value is a load error, not a silent
// fallback. The owning config's Validate reports negatives with the path.
type Duration time.Duration

// UnmarshalJSON decodes a JSON number as seconds (the legacy form), the
// string "off" as zero, and any other string as a Go duration.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s == "off" {
			*d = 0
			return nil
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n float64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("invalid duration: want a number of seconds or a duration string")
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

// MarshalJSON writes 0 as "off" and every whole-second value as a bare
// number of seconds, the legacy form; anything else goes out as a Go
// duration string, which loses nothing.
func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return []byte(`"off"`), nil
	}
	seconds := time.Duration(d).Seconds()
	if seconds >= 0 && seconds == math.Trunc(seconds) {
		return strconv.AppendInt(nil, int64(seconds), 10), nil
	}
	return json.Marshal(time.Duration(d).String())
}

// JSONSchema describes the accepted forms: a non-negative integer number
// of seconds, or a string that is either "off" or a Go duration.
func (Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "integer", Minimum: json.Number("0")},
			{Type: "string", Pattern: `^off$|^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`},
		},
	}
}

// durationForError renders a Duration for validation error messages: whole
// second values as a bare number, the way they are usually written, and
// anything else as a Go duration string.
func durationForError(d Duration) string {
	seconds := time.Duration(d).Seconds()
	if seconds == math.Trunc(seconds) {
		return strconv.FormatInt(int64(seconds), 10)
	}
	return time.Duration(d).String()
}

// IntOrOff is a config count knob that accepts a non-negative integer or
// the string "off". 0 or "off" disables the knob it configures, and a
// negative value is a load error, not a setting. The owning config's
// Validate reports negatives with the path.
type IntOrOff int

// UnmarshalJSON decodes a JSON integer, or the string "off" as zero.
func (v *IntOrOff) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s != "off" {
			return fmt.Errorf("invalid value %q: want an integer or \"off\"", s)
		}
		*v = 0
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("invalid value: want an integer or \"off\"")
	}
	*v = IntOrOff(n)
	return nil
}

// MarshalJSON writes 0 as "off" and every other value as its number.
func (v IntOrOff) MarshalJSON() ([]byte, error) {
	if v == 0 {
		return []byte(`"off"`), nil
	}
	return strconv.AppendInt(nil, int64(v), 10), nil
}

// JSONSchema describes the accepted forms: a non-negative integer, or the
// string "off".
func (IntOrOff) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "integer", Minimum: json.Number("0")},
			{Type: "string", Pattern: `^off$`},
		},
	}
}
