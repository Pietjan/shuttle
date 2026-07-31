package shuttle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/input"
)

// gallery accepts images.
type gallery struct {
	Base
	Received []string
	Sizes    []int64
	Saved    string
	dir      string
}

func (g *gallery) Uploads() []Upload {
	return []Upload{{
		Name:     "photos",
		MaxSize:  1 << 10, // small, so the limit is easy to cross in a test
		MaxFiles: 2,
		Accept:   []string{"image/png", "text/*"},
	}}
}

func (g *gallery) HandleUpload(_ context.Context, name string, files []*UploadedFile) error {
	if name != "photos" {
		return fmt.Errorf("unexpected upload %q", name)
	}
	for _, f := range files {
		g.Received = append(g.Received, f.Name)
		g.Sizes = append(g.Sizes, f.Size)

		if g.dir != "" {
			path, err := f.Save(g.dir)
			if err != nil {
				return err
			}
			g.Saved = path
		}
	}
	return nil
}

func (g *gallery) Render(ctx context.Context) templ.Component {
	box := input.New(FileInput(ctx, input.Attr, "photos"))
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := box.Render(ctx, w); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, `<p id="got">%s</p>`,
			templ.EscapeString(strings.Join(g.Received, ",")))
		return err
	})
}

// upload posts files the way the shim's XHR does.
func upload(t *testing.T, srv *httptest.Server, path string, files ...[3]string) (int, string) {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		name, contentType, content := f[0], f[1], f[2]

		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="files"; filename=%q`, name))
		h.Set("Content-Type", contentType)

		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		if _, err := io.WriteString(part, content); err != nil {
			t.Fatalf("multipart write: %v", err)
		}
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, &body)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// uploadURL digs the endpoint out of the rendered picker.
func uploadURL(t *testing.T, markup string) string {
	t.Helper()
	found, err := selectAll(markup, "[data-shuttle-upload]")
	if err != nil || len(found) == 0 {
		t.Fatalf("no upload input in %q", markup)
	}
	return attr(found[0], "data-shuttle-upload")
}

// TestFileInputRendersTheWiring.
func TestFileInputRendersTheWiring(t *testing.T) {
	sess := newSession("test", &gallery{})
	t.Cleanup(func() { sess.close(context.Background()) })

	markup, err := sess.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		`type="file"`,
		`id="shuttle-c-upload-photos"`,
		`accept="image/png,text/*"`,
		`multiple`, // MaxFiles is 2
		"/_shuttle/upload/test/c/photos",
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("missing %q in %q", want, markup)
		}
	}
}

// TestFileInputForAnUndeclaredUploadIsInert, rather than rendering a picker
// that posts to an endpoint which would refuse it.
func TestFileInputForAnUndeclaredUploadIsInert(t *testing.T) {
	sess := newSession("test", &gallery{})
	t.Cleanup(func() { sess.close(context.Background()) })

	_, _ = sess.Render(context.Background())

	sc := &scope{node: sess.root, table: map[string]Action{}, seen: map[string]bool{}}
	ctx := withScope(context.Background(), sc)
	got := render(t, input.New(FileInput(ctx, input.Attr, "not-declared")))

	if strings.Contains(got, "data-shuttle-upload") {
		t.Errorf("rendered a picker for an undeclared upload: %q", got)
	}
}

// TestUploadRoundTrip.
func TestUploadRoundTrip(t *testing.T) {
	c := &gallery{dir: t.TempDir()}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, sid := getPage(t, srv)
	stream := openStream(t, srv, sid)
	url := uploadURL(t, fragment(t, page))

	code, body := upload(t, srv, url, [3]string{"a.png", "image/png", "png bytes"})
	if code != http.StatusNoContent {
		t.Fatalf("upload: status %d (%s)", code, body)
	}

	if got := fmt.Sprint(c.Received); got != "[a.png]" {
		t.Errorf("received %v, want [a.png]", got)
	}
	if c.Sizes[0] != int64(len("png bytes")) {
		t.Errorf("size = %d, want %d", c.Sizes[0], len("png bytes"))
	}

	// The component re-renders afterwards, like any other piece of work.
	if evt := stream.event(t); !strings.Contains(evt, "a.png") {
		t.Errorf("no re-render after the upload: %q", evt)
	}

	// Save copied it somewhere durable before the temp file went away.
	saved, err := os.ReadFile(c.Saved)
	if err != nil {
		t.Fatalf("reading the saved file: %v", err)
	}
	if string(saved) != "png bytes" {
		t.Errorf("saved contents = %q", saved)
	}
}

// TestUploadedFilesAreCleanedUp: temp files that outlive the handler are a
// disk leak per upload.
func TestUploadedFilesAreCleanedUp(t *testing.T) {
	c := &keeper{}
	h := New(func() Component { return c })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)
	url := uploadURL(t, fragment(t, page))

	if code, body := upload(t, srv, url, [3]string{"a.txt", "text/plain", "hello"}); code != http.StatusNoContent {
		t.Fatalf("upload: status %d (%s)", code, body)
	}

	if c.path == "" {
		t.Fatal("handler never saw a file")
	}
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Errorf("temp file %q outlived the handler (err = %v)", c.path, err)
	}
}

// keeper remembers where its file was, to prove it is gone afterwards.
type keeper struct {
	Base
	path string
}

func (k *keeper) Uploads() []Upload { return []Upload{{Name: "doc"}} }

func (k *keeper) HandleUpload(_ context.Context, _ string, files []*UploadedFile) error {
	k.path = files[0].path
	return nil
}

func (k *keeper) Render(ctx context.Context) templ.Component {
	return input.New(FileInput(ctx, input.Attr, "doc"))
}

// TestUploadLimitsAreEnforcedServerSide. The picker was told the same
// rules, but a client that skips them has to be refused anyway.
func TestUploadLimitsAreEnforcedServerSide(t *testing.T) {
	for name, tc := range map[string]struct {
		files []([3]string)
		want  int
	}{
		"too large": {
			files: []([3]string){{"big.png", "image/png", strings.Repeat("x", 2<<10)}},
			want:  http.StatusRequestEntityTooLarge,
		},
		"too many": {
			files: []([3]string){
				{"a.png", "image/png", "one"},
				{"b.png", "image/png", "two"},
				{"c.png", "image/png", "three"},
			},
			want: http.StatusBadRequest,
		},
		"wrong type": {
			files: []([3]string){{"evil.exe", "application/x-msdownload", "MZ"}},
			want:  http.StatusBadRequest,
		},
		"nothing at all": {
			files: nil,
			want:  http.StatusBadRequest,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &gallery{}
			h := New(func() Component { return c })
			h.Logger = quietLogger()
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			page, _ := getPage(t, srv)
			url := uploadURL(t, fragment(t, page))

			if code, _ := upload(t, srv, url, tc.files...); code != tc.want {
				t.Errorf("status %d, want %d", code, tc.want)
			}
			if len(c.Received) != 0 {
				t.Errorf("a refused upload still reached the handler: %v", c.Received)
			}
		})
	}
}

// TestUploadAcceptMatching covers the wildcard forms on their own.
func TestUploadAcceptMatching(t *testing.T) {
	spec := Upload{Accept: []string{"image/png", "text/*"}}

	for got, want := range map[string]bool{
		"image/png":                true,
		"image/png; charset=utf-8": true,
		"text/plain":               true,
		"text/csv":                 true,
		"image/jpeg":               false,
		"application/pdf":          false,
		"":                         false,
	} {
		if spec.accepts(got) != want {
			t.Errorf("accepts(%q) = %v, want %v", got, !want, want)
		}
	}

	if !(Upload{}).accepts("anything/at-all") {
		t.Error("an empty Accept should accept anything")
	}
}

// TestUploadRejectsUnknownTargets.
func TestUploadRejectsUnknownTargets(t *testing.T) {
	h := New(func() Component { return &gallery{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, sid := getPage(t, srv)
	file := [3]string{"a.png", "image/png", "x"}

	for name, path := range map[string]string{
		"unknown session":   routePrefix + "/upload/deadbeef/c/photos",
		"unknown component": routePrefix + "/upload/" + sid + "/c-9/photos",
		"unknown upload":    routePrefix + "/upload/" + sid + "/c/nope",
	} {
		if code, _ := upload(t, srv, path, file); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, code)
		}
	}
}

// TestUploadNeedsAHandler: files arriving at a component that cannot
// receive them is a programming error worth saying out loud.
func TestUploadNeedsAHandler(t *testing.T) {
	h := New(func() Component { return &declaresOnly{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)
	url := uploadURL(t, fragment(t, page))

	code, body := upload(t, srv, url, [3]string{"a.txt", "text/plain", "x"})
	if code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
	if !strings.Contains(body, "UploadHandler") {
		t.Errorf("the failure does not say what is missing: %q", body)
	}
}

type declaresOnly struct{ Base }

func (d *declaresOnly) Uploads() []Upload { return []Upload{{Name: "doc"}} }

func (d *declaresOnly) Render(ctx context.Context) templ.Component {
	return input.New(FileInput(ctx, input.Attr, "doc"))
}

// TestSaveCleansTheClientFilename. The name comes from the client, and
// "../../etc/passwd" is exactly what an upload endpoint gets sent.
func TestSaveCleansTheClientFilename(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{
		"../../etc/passwd",
		`..\..\windows\system32\evil`,
		"/absolute/path",
		"plain.txt",
	} {
		src := filepath.Join(dir, "src")
		if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		f := &UploadedFile{Name: name, path: src}

		got, err := f.Save(dir)
		if err != nil {
			t.Fatalf("Save(%q): %v", name, err)
		}
		if filepath.Dir(got) != dir {
			t.Errorf("Save(%q) escaped to %q", name, got)
		}
	}
}

// TestShimUploadsWithXHR, since fetch cannot report progress.
func TestShimUploadsWithXHR(t *testing.T) {
	h := New(func() Component { return &gallery{} })
	h.Logger = quietLogger()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	page, _ := getPage(t, srv)

	for _, want := range []string{
		"XMLHttpRequest",
		"xhr.upload",
		"data-shuttle-upload",
		"data-shuttle-progress",
		"--shuttle-progress",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("shim missing %q", want)
		}
	}
}
