package shuttle

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/starfederation/datastar-go/datastar"
)

// Signaller is implemented by components that keep some state on the client
// rather than the server: an input's current value, whether a disclosure is
// open, a filter being typed. The map is the signal's name and its initial
// value.
//
// Shuttle emits them on the component's root, namespaced to this instance,
// and scopes every action's payload to that namespace. Both halves are
// necessary. Datastar has one global signal store with no scoping and no
// collision warning, so two components declaring "query" would silently
// share it - and every action sends every signal by default, so one
// unscoped click uploads the whole application's client state.
//
// Names must be plain identifiers: no dots, which are Datastar's path
// separator, and no hyphens, which it camel-cases.
type Signaller interface {
	Signals() map[string]any
}

var signalNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Signal returns the full dot-path of one of this component's signals -
// "c.query" - for use where Datastar wants a path rather than a reference.
// It returns "" outside a render pass.
func Signal(ctx context.Context, name string) string {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return ""
	}
	return sc.path().namespace() + "." + name
}

// Ref returns a signal as an expression reference - "$c.query" - which is
// how a component's own markup reads or writes it:
//
//	shuttle.On(input.Attr, "input", "@post('/search')")
//	text.New(shuttle.Ref(ctx, "query"))
func Ref(ctx context.Context, name string) string {
	if p := Signal(ctx, name); p != "" {
		return "$" + p
	}
	return ""
}

// Bind two-way binds a form control to one of this component's signals, so
// typing updates the client without a round trip:
//
//	input.New(shuttle.Bind(ctx, input.Attr, "query"))
//
// The value form of data-bind rather than the keyed form, because the value
// is taken as a literal signal path - the keyed form camel-cases its key,
// which would quietly rename anything hyphenated.
func Bind[O ~func(T), T any](ctx context.Context, attr Attrs[O], name string) O {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return func(T) {}
	}
	return bind(attr, pair{"data-bind", sc.path().namespace() + "." + name})
}

// signalAttrs renders a component's declared signals as attributes for its
// root element, namespaced and sorted so a re-render produces identical
// bytes.
//
// The keyed form (data-signals:<path>) rather than the object form, because
// that is the form whose __ifmissing modifier is documented - and
// __ifmissing is what stops a re-render from resetting client state to the
// defaults. A morph re-applies a data attribute whose value changed, so a
// component re-rendering with different initial values would otherwise
// overwrite whatever the user had just typed.
func signalAttrs(ns string, signals map[string]any) (string, error) {
	names := make([]string, 0, len(signals))
	for name := range signals {
		if !signalNameRE.MatchString(name) {
			return "", fmt.Errorf(
				"%w: signal name %q must be a plain identifier: Datastar reads dots as path separators and camel-cases hyphens",
				ErrBadComponent, name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		v, err := json.Marshal(signals[name])
		if err != nil {
			return "", fmt.Errorf("shuttle: signal %q: %w", name, err)
		}
		// JSON is a valid Datastar expression for the values worth defaulting
		// to, and Loom-style attribute escaping keeps the quotes intact.
		fmt.Fprintf(&b, ` data-signals:%s.%s__ifmissing="%s"`,
			ns, name, html.EscapeString(string(v)))
	}
	return b.String(), nil
}

// filterFor is the fetch option scoping an action's payload to one
// component's signals. Without it Datastar sends the entire global store on
// every event.
//
// The exclude half is what keeps a parent from also uploading its children:
// filterSignals matches whole dot-paths, so an include of /^c\./ catches
// c.1.query as readily as c.query. Excluding a numeric segment is precise
// because child namespaces are always numeric - which is the reason paths
// are numbered rather than named after their keys. An object-valued signal
// of the component's own, c.filters.name, is untouched by it.
func filterFor(ns string) string {
	q := regexp.QuoteMeta(ns)
	return fmt.Sprintf(`{filterSignals: {include: /^%s\./, exclude: /^%s\.[0-9]+\./}}`, q, q)
}

type signalsKey struct{}

// withSignals attaches the client values an action was invoked with. It
// carries the raw JSON rather than a decoded map, so a typed decode keeps
// the numbers it was sent instead of everything arriving as float64.
func withSignals(ctx context.Context, raw json.RawMessage) context.Context {
	return context.WithValue(ctx, signalsKey{}, raw)
}

func signalsFrom(ctx context.Context) json.RawMessage {
	raw, _ := ctx.Value(signalsKey{}).(json.RawMessage)
	return raw
}

// SignalValues returns the client-side signal values the running action was
// invoked with, namespace stripped, or nil if the client sent none.
//
// Convenient for a quick look; [DecodeSignals] is what a form should use.
// Either way these are client-controlled input: read what the component
// expects and convert it, and never reflect a payload onto struct fields
// wholesale - the mistake Livewire had to bolt #[Locked] onto.
func SignalValues(ctx context.Context) map[string]any {
	raw := signalsFrom(ctx)
	if len(raw) == 0 {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// DecodeSignals decodes the running action's signals into dest, which
// should be a pointer to a struct whose json tags name the component's
// signals. Signals the struct does not declare are ignored, which is what
// makes this the safe way to read them: the client can only reach fields
// the component asked for.
//
// A client that sent no signals leaves dest untouched.
func DecodeSignals(ctx context.Context, dest any) error {
	raw := signalsFrom(ctx)
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("%w: %w", errBadSignals, err)
	}
	return nil
}

// readSignals extracts a component's own slice of the signal payload.
//
// Datastar sends signals as an object nested to match their dot paths, so a
// component's values sit at the end of its path. An action posted by a
// component with no signals has no body at all, which is not an error -
// though note that only holds for POST, since a GET action puts its signals
// in the query string instead.
//
// The read has to happen before any SSE upgrade: the Go SDK closes the
// request body when it upgrades, and has a dedicated error for getting this
// backwards.
func readSignals(r *http.Request, p path) (json.RawMessage, error) {
	if r.ContentLength == 0 {
		return nil, nil
	}

	var raw map[string]json.RawMessage
	if err := datastar.ReadSignals(r, &raw); err != nil {
		return nil, fmt.Errorf("shuttle: decoding signals: %w", err)
	}

	// Descend to the component's namespace: signalRoot, then one level per
	// path index. A payload that does not reach this component is not an
	// error; it just means there is nothing here for it.
	cur, ok := raw[signalRoot]
	if !ok {
		return nil, nil
	}
	for _, i := range p {
		var level map[string]json.RawMessage
		if err := json.Unmarshal(cur, &level); err != nil {
			return nil, nil
		}
		if cur, ok = level[strconv.Itoa(i)]; !ok {
			return nil, nil
		}
	}
	return cur, nil
}
