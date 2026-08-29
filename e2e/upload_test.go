//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"testing"

	"github.com/a-h/templ"
	"github.com/mxschmitt/playwright-go"

	"github.com/pietjan/loom/input"
	"github.com/pietjan/shuttle"
)

// files takes uploads on their own request, which is the only way bytes go
// up: the stream is one-way.
type files struct {
	shuttle.Base
	Got []string
}

func (f *files) Uploads() []shuttle.Upload {
	return []shuttle.Upload{{
		Name:     "docs",
		MaxSize:  1 << 20,
		MaxFiles: 2,
		Accept:   []string{"text/*"},
	}}
}

func (f *files) HandleUpload(_ context.Context, _ string, uploaded []*shuttle.UploadedFile) error {
	for _, u := range uploaded {
		f.Got = append(f.Got, fmt.Sprintf("%s:%d", u.Name, u.Size))
	}
	return nil
}

func (f *files) Render(ctx context.Context) templ.Component {
	picker := input.New(
		shuttle.ID(ctx, input.ID, "docs"),
		shuttle.FileInput(ctx, input.Attr, "docs"),
	)

	got := f.Got
	return seq(picker, templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<ul data-got="%d">`, len(got))
		if err != nil {
			return err
		}
		for _, line := range got {
			if _, err := fmt.Fprintf(w, `<li>%s</li>`, templ.EscapeString(line)); err != nil {
				return err
			}
		}
		_, err = io.WriteString(w, `</ul>`)
		return err
	}))
}

// TestUploadTravelsOnItsOwnRequest drives the one piece of shuttle that
// leaves the stream entirely - the shim's XMLHttpRequest - and the state it
// leaves on the input while it runs, which is what a progress bar is drawn
// from.
func TestUploadTravelsOnItsOwnRequest(t *testing.T) {
	cmp := &files{}
	p := open(t, func() shuttle.Component { return cmp })

	picker := p.Locator(`input[type="file"]`)
	if err := picker.SetInputFiles([]playwright.InputFile{{
		Name:     "notes.txt",
		MimeType: "text/plain",
		// A buffer rather than a path: the browser is in a container and has
		// no view of this filesystem.
		Buffer: []byte("shuttle e2e upload"),
	}}); err != nil {
		t.Fatalf("choose a file: %v", err)
	}

	if err := expect(p.Locator(`[data-got="1"]`)).ToBeAttached(); err != nil {
		t.Fatalf("the upload never reached the component: %v", err)
	}
	if got := cmp.Got; len(got) != 1 || got[0] != "notes.txt:18" {
		t.Errorf("HandleUpload received %v, want the file and its 18 bytes", got)
	}

	// The shim clears its own state when the request finishes, so a second
	// upload starts from a clean input rather than from a stuck bar.
	if err := expect(picker).Not().ToHaveAttribute("data-shuttle-uploading", ""); err != nil {
		t.Errorf("the input still says it is uploading: %v", err)
	}
	if v, _ := picker.InputValue(); v != "" {
		t.Errorf("the input kept its file after the upload: %q", v)
	}
}

// TestUploadRefusesTheWrongType: the picker was told the rules, and the
// server enforces them anyway - a client's copy of a rule is a courtesy.
//
// The payload has to be real binary rather than a string that claims to be:
// the server checks the content, so text pretending to be an executable is
// text, and this component accepts text. The declared type is the lie in the
// client's favour, which is the case worth covering.
func TestUploadRefusesTheWrongType(t *testing.T) {
	cmp := &files{}
	p := open(t, func() shuttle.Component { return cmp })

	picker := p.Locator(`input[type="file"]`)
	if err := picker.SetInputFiles([]playwright.InputFile{{
		Name:     "sneaky.bin",
		MimeType: "text/plain",
		Buffer: []byte{
			'M', 'Z', 0x90, 0x00, 0x03, 0x00, 0x00, 0x00,
			0x04, 0x00, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00,
		},
	}}); err != nil {
		t.Fatalf("choose a file: %v", err)
	}

	if err := expect(picker).ToHaveAttribute("data-shuttle-upload-error",
		regexp.MustCompile(`unacceptable file type`)); err != nil {
		t.Errorf("a refused upload said nothing on the input: %v", err)
	}
	if len(cmp.Got) != 0 {
		t.Errorf("the server accepted %v", cmp.Got)
	}
}
