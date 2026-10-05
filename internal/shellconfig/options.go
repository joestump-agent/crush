package shellconfig

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// handleOption implements the `option` builtin.
//
// Usage: option <key> <value>
//
// Sets a single option field. The key is a kebab-case name; for list fields
// (context-path, disable-skill, etc.) each call appends to the list.
//
// "option reset <list-key>" wipes a list back to empty, dropping values set
// earlier in the script or via source. Values added after the reset are kept.
//
// Some config fields are phrased negatively (disable_metrics). Those are
// exposed positively — the user sets "metrics false" and it is stored as
// "disable_metrics true".
//
// Examples:
//
//	option data-directory .crush
//	option context-path .cursorrules
//	option reset skill-path
//	option metrics false
//	option debug true
//	option auto-lsp false
//	option request-timeout 300
//	option request-timeout 0
//
// Boolean shortcuts: for boolean fields, omitting the value sets it to true.
func handleOption(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	b := configBuilderFromCtx(ctx)
	if b == nil {
		return nil
	}
	if len(args) < 2 {
		return usage(stderr, "usage: option <key> [value]")
	}

	key := args[1]
	o := b.section("options")

	if key == "ui" {
		return optionUI(o, args, stderr)
	}

	// "option reset <key>" wipes a list back to empty. Because the builder
	// applies operations in execution order, this is just an assignment:
	// values added after the reset are kept, earlier ones are dropped.
	if key == "reset" {
		if len(args) < 3 {
			return usage(stderr, "usage: option reset <list-key>")
		}
		target := args[2]
		spec, ok := optionSpecs[target]
		if !ok {
			return usage(stderr, fmt.Sprintf("option: unknown key %q", target))
		}
		if spec.kind != optList {
			return usage(stderr, fmt.Sprintf("option: reset only applies to list options, %q is not one", target))
		}
		o[spec.jsonKey] = []any{}
		slog.Info("Option list reset in shell config", "key", target)
		return nil
	}

	// Determine the value.
	var val string
	if len(args) >= 3 {
		val = args[2]
	}

	if key == "attribution-trailer-style" {
		if val == "" {
			return usage(stderr, "option: attribution-trailer-style requires a value")
		}
		switch val {
		case "none", "co-authored-by", "assisted-by":
		default:
			return usage(stderr, fmt.Sprintf("option: attribution-trailer-style expects none, co-authored-by, or assisted-by, got %q", val))
		}
		attribution := childMap(o, "attribution")
		if _, ok := attribution["generated_with"]; !ok {
			attribution["generated_with"] = true
		}
		attribution["trailer_style"] = val
		slog.Info("Option set in shell config", "key", key, "value", val)
		return nil
	}

	if key == "attribution-generated-with" {
		bv := true
		if val != "" {
			parsed, err := parseBool(val)
			if err != nil {
				return usage(stderr, fmt.Sprintf("option: attribution-generated-with expects true/false, got %q", val))
			}
			bv = parsed
		}
		childMap(o, "attribution")["generated_with"] = bv
		slog.Info("Option set in shell config", "key", key, "value", bv)
		return nil
	}

	// The todo-*/dispatch-* keys write into the nested
	// options.todo_enforcement block, so they are a special case like
	// attribution-*, not entries in optionSpecs.
	if strings.HasPrefix(key, "todo-") || strings.HasPrefix(key, "dispatch-") {
		return handleTodoDispatchOption(key, val, o, stderr)
	}

	spec, ok := optionSpecs[key]
	if !ok {
		return usage(stderr, fmt.Sprintf("option: unknown key %q", key))
	}

	switch spec.kind {
	case optList:
		if val == "" {
			return usage(stderr, fmt.Sprintf("option: %s requires a value", key))
		}
		o[spec.jsonKey] = appendArr(o, spec.jsonKey, val)
		slog.Info("Option set in shell config", "key", key, "value", val)
		return nil

	case optBool:
		// If no value, default to true. Inverted keys store the negation,
		// so a positive key like "metrics" maps onto "disable_metrics".
		bv := true
		if val != "" {
			parsed, err := parseBool(val)
			if err != nil {
				return usage(stderr, fmt.Sprintf("option: %s expects true/false, got %q", key, val))
			}
			bv = parsed
		}
		if spec.inverted {
			bv = !bv
		}
		o[spec.jsonKey] = bv
		slog.Info("Option set in shell config", "key", key, "value", o[spec.jsonKey])
		return nil

	case optInt:
		if val == "" {
			return usage(stderr, fmt.Sprintf("option: %s requires a value", key))
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return usage(stderr, fmt.Sprintf("option: %s expects a number of seconds, got %q", key, val))
		}
		o[spec.jsonKey] = n
		slog.Info("Option set in shell config", "key", key, "value", n)
		return nil

	default: // optString
		if val == "" {
			return usage(stderr, fmt.Sprintf("option: %s requires a value", key))
		}
		o[spec.jsonKey] = val
		slog.Info("Option set in shell config", "key", key, "value", val)
		return nil
	}
}

// optionKind is the value type of a user-facing option key.
type optionKind int

const (
	optString optionKind = iota
	optBool
	optList
	optInt
)

// optionSpec describes one user-facing option key: the JSON field it writes,
// its value type, and (for booleans) whether the stored value is the inverse
// of what the user typed. Several config fields are phrased negatively
// (disable_metrics) but exposed positively (metrics), so "metrics false"
// stores "disable_metrics true".
type optionSpec struct {
	jsonKey  string
	kind     optionKind
	inverted bool
}

// optionSpecs maps user-facing kebab-case keys to their JSON field and type.
// This is the single source of truth for option key handling; the kind field
// drives parsing so there is no separate bool/list enumeration to drift out
// of sync.
//
// Not exhaustive by design: options with nested structure (option ui ...) or
// conditional logic (option attribution-...) are handled as special cases in
// handleOption above and do not appear here.
var optionSpecs = map[string]optionSpec{
	// Boolean fields (stored as-is).
	"debug":     {jsonKey: "debug", kind: optBool},
	"debug-lsp": {jsonKey: "debug_lsp", kind: optBool},
	"auto-lsp":  {jsonKey: "auto_lsp", kind: optBool},
	"progress":  {jsonKey: "progress", kind: optBool},

	// Boolean fields exposed positively but stored as their negation.
	"metrics":              {jsonKey: "disable_metrics", kind: optBool, inverted: true},
	"auto-summarize":       {jsonKey: "disable_auto_summarize", kind: optBool, inverted: true},
	"provider-auto-update": {jsonKey: "disable_provider_auto_update", kind: optBool, inverted: true},
	"default-providers":    {jsonKey: "disable_default_providers", kind: optBool, inverted: true},

	// String fields.
	"notifications":  {jsonKey: "notifications", kind: optString},
	"data-directory": {jsonKey: "data_directory", kind: optString},
	"initialize-as":  {jsonKey: "initialize_as", kind: optString},

	// Integer fields, in seconds.
	"request-timeout": {jsonKey: "request_timeout", kind: optInt},

	// List fields. Keys are singular because each call appends one value.
	"context-path":        {jsonKey: "context_paths", kind: optList},
	"global-context-path": {jsonKey: "global_context_paths", kind: optList},
	"skill-path":          {jsonKey: "skills_paths", kind: optList},
	"disable-skill":       {jsonKey: "disabled_skills", kind: optList},
}

// optionUI implements "option ui <key> <value>" for TUI-specific settings
// that live under options.tui rather than as top-level options.
func optionUI(options map[string]any, args []string, stderr io.Writer) error {
	if len(args) != 4 {
		return usage(stderr, "usage: option ui <compact|diff|transparent|mouse|scrollbar|working-dir-format|completions-max-depth|completions-max-items|exit-banner> <value>")
	}

	key := args[2]
	value := args[3]
	ui := childMap(options, "tui")

	switch key {
	case "compact", "transparent", "mouse":
		parsed, err := parseBool(value)
		if err != nil {
			return usage(stderr, fmt.Sprintf("option ui %s expects true/false, got %q", key, value))
		}
		jsonKey := key
		if key == "compact" {
			jsonKey = "compact_mode"
		}
		ui[jsonKey] = parsed
	case "diff":
		if value != "unified" && value != "split" {
			return usage(stderr, fmt.Sprintf("option ui diff expects unified or split, got %q", value))
		}
		ui["diff_mode"] = value
	case "scrollbar":
		if value != "default" && value != "always" && value != "never" {
			return usage(stderr, fmt.Sprintf("option ui scrollbar expects default, always, or never, got %q", value))
		}
		ui["scrollbar"] = value
	case "working-dir-format":
		value = strings.TrimSpace(value)
		if value == "" {
			return usage(stderr, "option ui working-dir-format requires a value, e.g. {user}@{host}:{cwd} ({cwd}, {user}, {host} placeholders)")
		}
		if !strings.Contains(value, "{cwd}") && !strings.Contains(value, "{user}") && !strings.Contains(value, "{host}") {
			return usage(stderr, fmt.Sprintf("option ui working-dir-format expects at least one of {cwd}, {user}, {host}, got %q", value))
		}
		ui["working_dir_format"] = value
	case "exit-banner":
		if value != "default" && value != "compact" && value != "none" {
			return usage(stderr, fmt.Sprintf("option ui exit-banner expects default, compact, or none, got %q", value))
		}
		ui["exit_banner"] = value
	case "completions-max-depth", "completions-max-items":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return usage(stderr, fmt.Sprintf("option ui %s expects a non-negative integer, got %q", key, value))
		}
		jsonKey := "max_depth"
		if key == "completions-max-items" {
			jsonKey = "max_items"
		}
		childMap(ui, "completions")[jsonKey] = parsed
	default:
		return usage(stderr, fmt.Sprintf("option ui: unknown key %q", key))
	}

	slog.Info("UI option set in shell config", "key", key, "value", value)
	return nil
}

// todoDispatchKind is the value type of a todo-*/dispatch-* option key.
type todoDispatchKind int

const (
	todoDispatchBool todoDispatchKind = iota
	todoDispatchCount
	todoDispatchDuration
)

// todoDispatchOption describes one todo-*/dispatch-* key: the field it
// writes under options.todo_enforcement and how its value is parsed.
type todoDispatchOption struct {
	jsonKey string
	kind    todoDispatchKind
	// min is the smallest accepted count; a value below it is a load
	// error. todo-nudge-threshold requires at least 1 because 0 would
	// silently disable nudging; kill-after-nudges accepts 0 as "off".
	min int
}

// todoDispatchOptions maps the todo-*/dispatch-* keys to their field under
// options.todo_enforcement (#403). It is the single source of truth for
// these keys, handled as a special case in handleOption because they write
// a nested block rather than a top-level option.
var todoDispatchOptions = map[string]todoDispatchOption{
	"todo-nudge":             {jsonKey: "enabled", kind: todoDispatchBool},
	"todo-nudge-threshold":   {jsonKey: "nudge_threshold", kind: todoDispatchCount, min: 1},
	"todo-hard-gate":         {jsonKey: "hard_gate", kind: todoDispatchBool},
	"todo-kill-after-nudges": {jsonKey: "kill_after_nudges", kind: todoDispatchCount, min: 0},
	"dispatch-stall":         {jsonKey: "stall_window", kind: todoDispatchDuration},
	"dispatch-timeout":       {jsonKey: "hard_timeout", kind: todoDispatchDuration},
}

// handleTodoDispatchOption implements the todo-*/dispatch-* family. Every
// key writes into options.todo_enforcement. Durations accept a Go duration
// ("5m") or bare integer seconds; counts and durations both accept "off"
// for zero. Values are stored as whole seconds or integers, the forms the
// config's Duration and IntOrOff fields decode.
func handleTodoDispatchOption(key, val string, options map[string]any, stderr io.Writer) error {
	spec, ok := todoDispatchOptions[key]
	if !ok {
		return usage(stderr, fmt.Sprintf("option: unknown key %q", key))
	}
	target := childMap(options, "todo_enforcement")

	switch spec.kind {
	case todoDispatchBool:
		// Boolean shortcuts match the flat boolean keys: omitting the
		// value sets the field to true.
		bv := true
		if val != "" {
			parsed, err := parseBool(val)
			if err != nil {
				return usage(stderr, fmt.Sprintf("option: %s expects true, false, on, or off, got %q", key, val))
			}
			bv = parsed
		}
		target[spec.jsonKey] = bv
		slog.Info("Option set in shell config", "key", key, "value", bv)
		return nil

	case todoDispatchCount:
		if val == "" {
			return usage(stderr, fmt.Sprintf("option: %s requires a value", key))
		}
		n, err := parseCountOrOff(val)
		if err != nil {
			return usage(stderr, fmt.Sprintf("option: %s: %v", key, err))
		}
		if n < spec.min {
			return usage(stderr, fmt.Sprintf("option: %s must be at least %d, got %d", key, spec.min, n))
		}
		target[spec.jsonKey] = n
		slog.Info("Option set in shell config", "key", key, "value", n)
		return nil

	default: // todoDispatchDuration
		if val == "" {
			return usage(stderr, fmt.Sprintf("option: %s requires a value", key))
		}
		seconds, err := parseSecondsOrOff(val)
		if err != nil {
			return usage(stderr, fmt.Sprintf("option: %s: %v", key, err))
		}
		target[spec.jsonKey] = seconds
		slog.Info("Option set in shell config", "key", key, "value", seconds)
		return nil
	}
}

// parseCountOrOff parses a count option value: the string "off" is zero,
// anything else must be a non-negative integer.
func parseCountOrOff(val string) (int, error) {
	if strings.EqualFold(val, "off") {
		return 0, nil
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("expects an integer or off, got %q", val)
	}
	if n < 0 {
		return 0, fmt.Errorf("expects a non-negative integer or off, got %d", n)
	}
	return n, nil
}

// parseSecondsOrOff parses a duration option value: the string "off" is
// zero, a bare integer is seconds, anything else is a Go duration string.
// The result is whole seconds, the form the config's Duration field
// accepts.
func parseSecondsOrOff(val string) (int, error) {
	if strings.EqualFold(val, "off") {
		return 0, nil
	}
	if n, err := strconv.Atoi(val); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("expects a non-negative number of seconds or off, got %d", n)
		}
		return n, nil
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("expects a duration (5m), a number of seconds (300), or off, got %q", val)
	}
	if d < 0 {
		return 0, fmt.Errorf("expects a non-negative duration, got %q", val)
	}
	return int(d / time.Second), nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", s)
	}
}
