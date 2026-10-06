// Extension registry for declared, statically typed TaskStatusUpdateEvent
// metadata (#359): each A2A extension names a metadata key (its URI), a
// description, and the concrete Go type and JSON Schema of the value it
// carries.
//
// The server encodes extension values with [Encode] — a JSON round trip that
// keeps the value JSON-shaped, which the SDK's task store requires — and the
// client decodes them with [Decode]/[DecodeValue] after checking that the
// remote agent's card declared the extension. The registry is the single
// source both sides consult, so the card's advertised extensions and the
// client's activation requests never drift apart.
package a2a

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/invopop/jsonschema"

	"github.com/charmbracelet/crush/internal/agent"
)

// extensionURIPrefix is the crush-owned namespace extensions declare
// themselves under, so a URI identifies the extension without a registry
// lookup.
const extensionURIPrefix = "https://crush.charm.land/ext/"

// Extension is one declared, statically typed A2A metadata extension: Task
// metadata keyed by URI carries values of Type, described by Schema.
type Extension struct {
	// URI is the extension's unique identifier and the metadata key its
	// values are stored under. Must start with [extensionURIPrefix].
	URI string
	// Description is the one-liner advertised on the agent card.
	Description string
	// Type is the Go type an extension value decodes into. Required.
	Type reflect.Type
	// Schema is the JSON Schema of an extension value, advertised in the
	// card's extension params so third-party clients can validate values.
	// Optional.
	Schema *jsonschema.Schema
}

// registry is the process-wide extension registry, populated at init by
// mustRegister and read on every card build, stream, and metadata decode.
var registry = map[string]Extension{}

// Register adds an extension to the registry. It rejects an empty URI, a URI
// outside [extensionURIPrefix], a missing Type, and a duplicate URI.
func Register(ext Extension) error {
	if ext.URI == "" {
		return fmt.Errorf("a2a: extension URI is empty")
	}
	if len(ext.URI) < len(extensionURIPrefix) || ext.URI[:len(extensionURIPrefix)] != extensionURIPrefix {
		return fmt.Errorf("a2a: extension URI %q is outside the %s namespace", ext.URI, extensionURIPrefix)
	}
	if ext.Type == nil {
		return fmt.Errorf("a2a: extension %s has no type", ext.URI)
	}
	if _, exists := registry[ext.URI]; exists {
		return fmt.Errorf("a2a: extension %s is already registered", ext.URI)
	}
	registry[ext.URI] = ext
	return nil
}

// mustRegister registers a statically known extension, panicking on error:
// a bad static registration is a programming error, not a runtime condition.
func mustRegister(ext Extension) {
	if err := Register(ext); err != nil {
		panic(err)
	}
}

// Registered returns the registry's extensions sorted by URI.
func Registered() []Extension {
	out := make([]Extension, 0, len(registry))
	for _, ext := range registry {
		out = append(out, ext)
	}
	slices.SortFunc(out, func(a, b Extension) int {
		return cmp.Compare(a.URI, b.URI)
	})
	return out
}

// Lookup returns the extension registered under uri.
func Lookup(uri string) (Extension, bool) {
	ext, ok := registry[uri]
	return ext, ok
}

// Encode converts an extension value into the JSON-shaped form A2A metadata
// and the SDK's task store require (nil, bools, numbers, strings, and their
// slices/maps): a typed Go value is silently valid to SetMeta and fatal one
// layer down, failing the task-state save and the whole task.
func Encode(ext Extension, v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("a2a: encode %s extension value: %w", ext.URI, err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("a2a: normalize %s extension value: %w", ext.URI, err)
	}
	return out, nil
}

// DecodeValue decodes one raw metadata value into a freshly allocated
// pointer to the extension's Type. Use it when the concrete type is not
// known at compile time; [Decode] is the typed path.
func DecodeValue(ext Extension, raw any) (any, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("a2a: re-encode %s extension value: %w", ext.URI, err)
	}
	out := reflect.New(ext.Type).Interface()
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("a2a: decode %s extension value: %w", ext.URI, err)
	}
	return out, nil
}

// Decode decodes the metadata value stored under the extension's URI into
// T. T must be either the extension's Type or a pointer to it. A missing
// key is an error — a declared extension whose values are always present
// should use this, and treat the error as a protocol violation.
func Decode[T any](meta map[string]any, ext Extension) (T, error) {
	var zero T
	raw, ok := meta[ext.URI]
	if !ok {
		return zero, fmt.Errorf("a2a: metadata has no %s extension value", ext.URI)
	}
	decoded, err := DecodeValue(ext, raw)
	if err != nil {
		return zero, err
	}
	val := reflect.ValueOf(decoded)
	outType := reflect.TypeFor[T]()
	if val.Type() != outType {
		// DecodeValue allocates a pointer; unwrap it for a non-pointer T.
		if val.Kind() == reflect.Pointer && outType.Kind() != reflect.Pointer && val.Type().Elem() == outType {
			val = val.Elem()
		} else {
			return zero, fmt.Errorf("a2a: extension %s decodes into %s, not %s", ext.URI, val.Type(), outType)
		}
	}
	return val.Interface().(T), nil
}

// TodoExtensionURI is the URI of the todos/v1 extension: structured dispatch
// todo progress, carried in TaskStatusUpdateEvent metadata (#174, #359).
const TodoExtensionURI = extensionURIPrefix + "todos/v1"

// TodoExt is the todos/v1 extension's registry entry.
var TodoExt = Extension{
	URI:         TodoExtensionURI,
	Description: "Structured dispatch todo progress in TaskStatusUpdateEvent metadata.",
	Type:        reflect.TypeFor[agent.TodoProgress](),
	Schema:      new(jsonschema.Reflector).Reflect(agent.TodoProgress{}),
}

// The statically known extensions, registered at init so every card, stream,
// and decode sees them.
func init() {
	mustRegister(TodoExt)
}

// cardExtensions derives the agent card's advertised extension list from the
// registry. Extensions are always optional (Required false): a consumer that
// ignores them still gets the message text. Each extension's JSON Schema
// travels in its params under "schema" so third-party clients can discover
// and validate the metadata shape.
func cardExtensions() []a2aspec.AgentExtension {
	registered := Registered()
	out := make([]a2aspec.AgentExtension, 0, len(registered))
	for _, ext := range registered {
		e := a2aspec.AgentExtension{
			URI:         ext.URI,
			Description: ext.Description,
			Required:    false,
		}
		if ext.Schema != nil {
			if params, ok := schemaParams(ext); ok {
				e.Params = params
			}
		}
		out = append(out, e)
	}
	return out
}

// schemaParams marshals an extension's JSON Schema into the JSON-shaped
// params map the card stores. A schema that fails to round trip is a bug,
// logged and skipped rather than failing card construction.
func schemaParams(ext Extension) (map[string]any, bool) {
	b, err := json.Marshal(ext.Schema)
	if err != nil {
		slog.Warn("A2A extension schema failed to marshal; card params skipped", "uri", ext.URI, "err", err)
		return nil, false
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		slog.Warn("A2A extension schema failed to normalize; card params skipped", "uri", ext.URI, "err", err)
		return nil, false
	}
	return map[string]any{"schema": schema}, true
}
