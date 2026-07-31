package shuttle

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/pietjan/loom"
	"github.com/pietjan/loom/button"
	"github.com/pietjan/loom/card"
	"github.com/pietjan/loom/input"
)

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(loom.NewContext(context.Background()), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestOnInfersPerPackageOptionType is the spike's real assertion. Each of
// these three calls instantiates On at a different Option type, inferred
// from that package's Attr, with no type arguments written by hand. If this
// compiles, the binding API works across all 48 component packages.
func TestOnInfersPerPackageOptionType(t *testing.T) {
	got := render(t, button.New(
		button.Primary,
		On(button.Attr, "click", "@post('/inc')"),
	))
	if !strings.Contains(got, `data-on:click="@post(&#39;/inc&#39;)"`) {
		t.Errorf("button: binding missing from %q", got)
	}

	got = render(t, card.New(On(card.Attr, "mouseenter", "$hover = true")))
	if !strings.Contains(got, `data-on:mouseenter`) {
		t.Errorf("card: binding missing from %q", got)
	}

	got = render(t, input.New(On(input.Attr, "input", "@post('/search')")))
	if !strings.Contains(got, `data-on:input`) {
		t.Errorf("input: binding missing from %q", got)
	}
}

// TestOneOptionCarriesManyAttributes covers the constraint that forced the
// single-return design: a binding that needs two attributes still arrives as
// one value, so it can sit alongside other options in the same call.
func TestOneOptionCarriesManyAttributes(t *testing.T) {
	got := render(t, button.New(
		button.Primary,
		button.Small,
		bind(button.Attr,
			pair{"data-on:click", "@post('/save')"},
			pair{"data-indicator", "saving"},
		),
	))
	for _, want := range []string{`data-on:click`, `data-indicator="saving"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// TestBindingDoesNotDisturbTheComponent guards the thing that would make
// this whole approach unusable: options must compose, not collide. The
// component keeps its recipe classes and its data-ui marker.
func TestBindingDoesNotDisturbTheComponent(t *testing.T) {
	plain := render(t, button.New(button.Primary))
	bound := render(t, button.New(button.Primary, On(button.Attr, "click", "@post('/x')")))

	if !strings.Contains(bound, `data-ui="button"`) {
		t.Errorf("marker lost: %q", bound)
	}
	if strings.Contains(plain, "class=") && !strings.Contains(bound, "class=") {
		t.Errorf("recipe classes lost:\nplain %q\nbound %q", plain, bound)
	}
}
