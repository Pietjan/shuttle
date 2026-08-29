package shuttle

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/field"
	"github.com/pietjan/loom/input"
)

// signup is a form the way one is meant to be written: client-side signals
// for what is being typed, server-side state for what has been committed,
// and validation that runs on both halves of the change/submit split.
type signup struct {
	Base

	Email    string // committed
	Saved    bool
	Errors   Validation
	Attempts int
}

func (s *signup) Signals() map[string]any {
	return map[string]any{"email": ""}
}

// fields is the typed view of this component's signals.
type fields struct {
	Email string `json:"email"`
	Age   int    `json:"age"`
}

func (s *signup) read(ctx context.Context) (fields, error) {
	var f fields
	return f, DecodeSignals(ctx, &f)
}

// validate runs on every keystroke without committing anything.
func (s *signup) validate(ctx context.Context) error {
	f, err := s.read(ctx)
	if err != nil {
		return err
	}

	s.Errors = Validate()
	s.Errors.Require("email", f.Email, "Email is required")
	if f.Email != "" && !strings.Contains(f.Email, "@") {
		s.Errors.Add("email", "That does not look like an email address")
	}
	return nil
}

// submit commits, but only if the same rules pass.
func (s *signup) submit(ctx context.Context) error {
	s.Attempts++
	if err := s.validate(ctx); err != nil {
		return err
	}
	if !s.Errors.OK() {
		return nil
	}

	f, _ := s.read(ctx)
	s.Email, s.Saved = f.Email, true
	return nil
}

func (s *signup) Render(ctx context.Context) templ.Component {
	box := input.New(
		ID(ctx, input.ID, "email"),
		Bind(ctx, input.Attr, "email"),
		// Rendering value= explicitly is not optional: the morph compares the
		// value *attribute*, so omitting it clears whatever was typed.
		input.Value(s.Email),
		OnChange(ctx, input.Attr, 300*time.Millisecond, s.validate),
	)
	save := button.New(
		button.Primary,
		OnClick(ctx, button.Attr, s.submit),
	)

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		wrap := field.Root(field.Error(s.Errors.For("email")))
		if err := wrap.Render(templ.WithChildren(ctx, box), w); err != nil {
			return err
		}
		return save.Render(templ.WithChildren(ctx, templ.Raw("save")), w)
	})
}

// urls returns this component's action endpoints in document order:
// the debounced change binding, then the submit.
func (s *signup) urls(t *testing.T, markup string) (change, submit string) {
	t.Helper()
	changeRE := `data-on:input__debounce.300ms="@post\(&#39;([^&]+)&#39;`
	m := regexpMustFind(t, changeRE, markup)
	return m, clickURLs(t, markup)[0]
}

func regexpMustFind(t *testing.T, pattern, s string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %q in %q", pattern, s)
	}
	return m[1]
}

// TestChangeAndSubmitAreSeparateBindings. Validating and committing are
// different actions, and a form that cannot tell them apart either saves
// half-typed input or refuses to say what is wrong until submit.
func TestChangeAndSubmitAreSeparateBindings(t *testing.T) {
	sess := newSession("test", &signup{})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(markup, "data-on:input__debounce.300ms=") {
		t.Errorf("change binding missing or not debounced: %q", markup)
	}
	// Per keystroke without a debounce is one request per character.
	if strings.Contains(markup, `data-on:input="`) {
		t.Errorf("change binding is undebounced: %q", markup)
	}
}

// TestSubmitPreventsTheBrowserSubmit: without __prevent the browser also
// submits the form normally and navigates away from the live session.
func TestSubmitPreventsTheBrowserSubmit(t *testing.T) {
	sess := newSession("test", &probe{
		t: t,
		check: func(t *testing.T, ctx context.Context) {
			got := render(t, button.New(OnSubmit(ctx, button.Attr, func(context.Context) error {
				return nil
			})))
			if !strings.Contains(got, "data-on:submit__prevent=") {
				t.Errorf("submit binding does not prevent the default: %q", got)
			}
		},
	})
	if _, err := sess.Render(context.Background()); err != nil {
		t.Fatalf("render: %v", err)
	}
}

// TestValidationFlowsIntoFieldError covers the Loom seam: field.Error is a
// no-op on the empty string, so a validation result can be passed through
// unconditionally rather than wrapped in a conditional.
func TestValidationFlowsIntoFieldError(t *testing.T) {
	valid := &signup{Errors: Validate()}
	sess := newSession("test", valid)
	clean, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(clean, `data-ui="field-error"`) {
		t.Errorf("a valid form rendered an error: %q", clean)
	}

	invalid := &signup{Errors: Validation{"email": "Email is required"}}
	sess = newSession("test", invalid)
	broken, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		`data-ui="field-error"`,
		"Email is required",
		`aria-invalid="true"`, // wired by field, not by us
	} {
		if !strings.Contains(broken, want) {
			t.Errorf("missing %q in %q", want, broken)
		}
	}
}

// TestInputRendersItsValue is the morph trap that eats user input: the
// morph compares the value *attribute*, not the property, so a re-render
// that omits value= clears the live field.
func TestInputRendersItsValue(t *testing.T) {
	sess := newSession("test", &signup{Email: "already@saved.example"})
	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(markup, `value="already@saved.example"`) {
		t.Errorf("input did not render its value attribute: %q", markup)
	}
}

// TestValidationRules.
func TestValidationRules(t *testing.T) {
	v := Validate()
	if !v.OK() {
		t.Error("a fresh Validation is not OK")
	}

	v.Require("email", "", "required")
	v.Add("email", "second message")
	v.Add("name", "also bad")

	if v.OK() {
		t.Error("OK after two failures")
	}
	// First message wins, so a rule chain reports the specific failure.
	if got := v.For("email"); got != "required" {
		t.Errorf("For(email) = %q, want required", got)
	}
	if got := v.For("absent"); got != "" {
		t.Errorf("For(absent) = %q, want empty", got)
	}
	if !v.Invalid("email") || v.Invalid("absent") {
		t.Error("Invalid disagrees with For")
	}
	if got := fmt.Sprint(v.Fields()); got != "[email name]" {
		t.Errorf("Fields = %v, want [email name]", got)
	}

	v.Require("ok", "a value", "required")
	if v.Invalid("ok") {
		t.Error("Require flagged a non-empty value")
	}
}

// TestFormRoundTrip drives the whole split over the transport: type
// something invalid and get a message without committing, then fix it and
// submit.
func TestFormRoundTrip(t *testing.T) {
	c := &signup{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)

	// Each action's markup names the endpoints for the next one, so the
	// URLs come off the patch every time - as they would in a browser,
	// where the morph is what replaces them.
	markup := fragment(t, page)
	change, _ := c.urls(t, markup)

	// The submit endpoint is read off each patch below, not from the first
	// render: a morph replaces it every time.
	var submit string

	// Typing something that fails validation reports, but commits nothing.
	if code := postBody(t, srv, sid, change, `{"c":{"email":"nope"}}`); code != http.StatusNoContent {
		t.Fatalf("change: status %d", code)
	}
	if c.Errors.OK() {
		t.Error("invalid input passed validation")
	}
	if c.Email != "" || c.Saved {
		t.Errorf("change committed state: email=%q saved=%v", c.Email, c.Saved)
	}

	markup = stream.event(t)
	if !strings.Contains(markup, "That does not look like an email address") {
		t.Errorf("the message never reached the page: %q", markup)
	}
	_, submit = c.urls(t, markup)

	// Submitting it still refuses - and patches nothing, because the page
	// already shows the message. Unchanged markup keeps its generation,
	// so the endpoints it names stay valid for the retry below.
	if code := postBody(t, srv, sid, submit, `{"c":{"email":"nope"}}`); code != http.StatusNoContent {
		t.Fatalf("bad submit: status %d", code)
	}
	if c.Saved {
		t.Error("submit committed invalid input")
	}

	// Fixing it and submitting commits.
	if code := postBody(t, srv, sid, submit, `{"c":{"email":"real@example.com"}}`); code != http.StatusNoContent {
		t.Fatalf("good submit: status %d", code)
	}
	if !c.Saved || c.Email != "real@example.com" {
		t.Errorf("submit did not commit: email=%q saved=%v", c.Email, c.Saved)
	}
	if c.Attempts != 2 {
		t.Errorf("submit ran %d times, want 2", c.Attempts)
	}
}

// TestDecodeSignalsIsTyped: a struct with json tags, so numbers stay
// numbers rather than arriving as float64 through map[string]any.
func TestDecodeSignalsIsTyped(t *testing.T) {
	ctx := withSignals(context.Background(), []byte(`{"email":"a@b.c","age":41,"extra":"ignored"}`))

	var f fields
	if err := DecodeSignals(ctx, &f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Email != "a@b.c" {
		t.Errorf("Email = %q", f.Email)
	}
	if f.Age != 41 {
		t.Errorf("Age = %d, want 41", f.Age)
	}
}

// TestDecodeSignalsIgnoresWhatTheStructDoesNotDeclare is the security
// property: the client can only reach fields the component asked for.
func TestDecodeSignalsIgnoresWhatTheStructDoesNotDeclare(t *testing.T) {
	type limited struct {
		Email string `json:"email"`
	}
	ctx := withSignals(context.Background(),
		[]byte(`{"email":"a@b.c","Saved":true,"admin":true}`))

	var l limited
	if err := DecodeSignals(ctx, &l); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if l.Email != "a@b.c" {
		t.Errorf("Email = %q", l.Email)
	}
}

// TestDecodeSignalsWithoutAPayload leaves dest alone rather than erroring,
// so an action bound on a component with no signals still works.
func TestDecodeSignalsWithoutAPayload(t *testing.T) {
	f := fields{Email: "untouched"}
	if err := DecodeSignals(context.Background(), &f); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Email != "untouched" {
		t.Errorf("dest was modified: %q", f.Email)
	}
}

// TestDecodeSignalsRejectsRubbish, and reports it as a bad request rather
// than a server fault.
func TestDecodeSignalsRejectsRubbish(t *testing.T) {
	ctx := withSignals(context.Background(), []byte(`{"age":"not a number"}`))

	var f fields
	err := DecodeSignals(ctx, &f)
	if err == nil {
		t.Fatal("decode accepted a string for an int field")
	}
	if !strings.Contains(err.Error(), "bad signals") {
		t.Errorf("err = %v, want it to wrap errBadSignals", err)
	}
}
