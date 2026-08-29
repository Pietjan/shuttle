package shuttle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	slashpath "path"
	"path/filepath"
	"strings"
)

// Uploads are their own subsystem rather than another kind of action, and
// they have to be: the stream is one-way, so bytes cannot go up it, and
// fetch reports no upload progress at all - Datastar's own Pro feature for
// it was withdrawn over browser support. So files travel over their own
// multipart request, driven by XMLHttpRequest, which is the only thing in
// a browser that will say how far along an upload is.
//
// Files stream to temp files rather than into memory. A component holding
// server-side state is already paying per connected tab; buffering whole
// uploads on top of that turns one large file into an outage.

// Upload declares one thing a component will accept.
type Upload struct {
	// Name identifies the upload, and must be a plain identifier: it
	// becomes part of an element id and a URL.
	Name string

	// MaxSize is the largest single file accepted, in bytes. Zero means
	// DefaultMaxUploadSize.
	MaxSize int64

	// MaxFiles is how many may arrive at once. Zero means one.
	MaxFiles int

	// Accept lists acceptable content types, as full types ("image/png") or
	// wildcards ("image/*"). Empty accepts anything.
	//
	// It reaches the file picker as its accept attribute, but that is only a
	// hint to the user. What is enforced is the type detected from the
	// file's own first bytes - not the one the client declared, which is a
	// string an attacker writes, and which would make this check a
	// formality: an executable labelled image/png would pass it.
	Accept []string

	// TrustDeclaredType checks the client's declared content type against
	// Accept instead of the one detected from the bytes.
	//
	// The detection is signature-based, so a container format arrives as its
	// container: a .docx is a zip and a .csv is text/plain. The second is
	// handled - any text/* entry accepts detected text - but the first is
	// not, and cannot be without opening the archive. Set this for those
	// formats, and know that the declared type is a string the client
	// writes: for that upload, Accept keeps honest files tidy rather than
	// keeping hostile ones out.
	TrustDeclaredType bool
}

// DefaultMaxUploadSize is the per-file limit when an Upload leaves MaxSize
// unset. A limit has to exist: without one, an upload endpoint is a way to
// fill the disk.
const DefaultMaxUploadSize = 32 << 20 // 32 MiB

func (u Upload) maxSize() int64 {
	if u.MaxSize <= 0 {
		return DefaultMaxUploadSize
	}
	return u.MaxSize
}

func (u Upload) maxFiles() int {
	if u.MaxFiles <= 0 {
		return 1
	}
	return u.MaxFiles
}

// accepts reports whether a content type is allowed.
func (u Upload) accepts(contentType string) bool {
	if len(u.Accept) == 0 {
		return true
	}

	got, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		got = strings.ToLower(strings.TrimSpace(contentType))
	}
	for _, want := range u.Accept {
		want = strings.ToLower(strings.TrimSpace(want))
		switch {
		case want == got:
			return true
		case strings.HasSuffix(want, "/*") &&
			strings.HasPrefix(got, strings.TrimSuffix(want, "*")):
			return true
		}
	}
	return false
}

// Uploader is implemented by components that accept files.
type Uploader interface {
	Uploads() []Upload
}

// UploadHandler receives files once they have arrived and been checked. It
// runs on the session's goroutine, like every other piece of component
// work, and the component re-renders afterwards.
//
// The files are deleted when it returns, so anything worth keeping has to
// be copied out - see [UploadedFile.Save].
type UploadHandler interface {
	HandleUpload(ctx context.Context, name string, files []*UploadedFile) error
}

// UploadedFile is one received file, waiting in temporary storage.
type UploadedFile struct {
	// Name is the filename the client sent. It is client-controlled input:
	// never join it onto a path without cleaning it, which is what Save
	// does.
	Name string
	// Size is the number of bytes received.
	Size int64
	// Type is the content type detected from the file's own first bytes,
	// which is what Accept was checked against. Trustworthy, and coarse:
	// see [Upload.TrustDeclaredType].
	Type string
	// DeclaredType is what the client said it was sending. Client-controlled
	// input like Name: useful for a filename hint or a log line, never a
	// basis for a decision.
	DeclaredType string

	path string
}

// Open reads the received file.
func (f *UploadedFile) Open() (*os.File, error) {
	// #nosec G304 -- the path is a temp file this package created; the
	// client never names it, and Name is only ever used by Save.
	return os.Open(f.path)
}

// Save copies the file to dst, which is a directory. It returns the path
// written.
//
// The client's filename is reduced to its base and stripped of separators
// before use: a name like "../../etc/passwd" is exactly what an upload
// endpoint gets sent.
func (f *UploadedFile) Save(dir string) (string, error) {
	// The path package, not filepath: this cleans the *client's* name, and
	// the client's separators do not change with the server's OS. filepath
	// here made "/absolute/path" into a UNC volume root on Windows, whose
	// Base is the separator - and the "file" it then saved was dir itself.
	name := slashpath.Base(slashpath.Clean("/" + strings.ReplaceAll(f.Name, `\`, "/")))
	// A drive-relative Windows name ("C:evil") survives Base; the colon and
	// everything before it goes too, or the write lands somewhere a colon
	// means something.
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	if name == "." || name == "/" || name == "" {
		name = "upload"
	}

	dst := filepath.Join(dir, name)
	src, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()

	// #nosec G304 G703 -- name was reduced to a base above with separators
	// stripped, so dst cannot escape dir; TestSaveCleansTheClientFilename
	// covers traversal explicitly.
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, src); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return dst, nil
}

// Errors an upload can fail with, all of them the client's doing.
var (
	// ErrNoUpload means the component declares no upload by that name.
	ErrNoUpload = errors.New("shuttle: no such upload")

	// ErrTooManyFiles means more files arrived than the Upload allows.
	ErrTooManyFiles = errors.New("shuttle: too many files")

	// ErrFileTooLarge means a file exceeded the Upload's MaxSize.
	ErrFileTooLarge = errors.New("shuttle: file too large")

	// ErrFileType means a file's content type is not accepted.
	ErrFileType = errors.New("shuttle: unacceptable file type")

	// ErrNoUploadHandler means files arrived at a component that never
	// implemented HandleUpload, so nothing would have read them.
	ErrNoUploadHandler = errors.New("shuttle: component does not implement UploadHandler")
)

// uploadFor finds a component's declaration by name.
func uploadFor(cmp Component, name string) (Upload, bool) {
	u, ok := cmp.(Uploader)
	if !ok {
		return Upload{}, false
	}
	for _, spec := range u.Uploads() {
		if spec.Name == name {
			return spec, true
		}
	}
	return Upload{}, false
}

// receive streams one part to a temp file, refusing anything over the
// limit rather than reading it and complaining afterwards.
func receive(part io.Reader, filename, contentType string, spec Upload) (*UploadedFile, error) {
	// Sniff before anything is written. http.DetectContentType reads at most
	// 512 bytes, so a file of the wrong kind is refused having cost that
	// much rather than a temp file the size of whatever was sent.
	head := make([]byte, 512)
	n0, err := io.ReadFull(part, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	head = head[:n0]

	detected := http.DetectContentType(head)
	if spec.TrustDeclaredType {
		// The declared type substitutes for the detected one - it does not
		// waive the check. A flag that quietly accepted everything would
		// turn "trust the label on containers" into "no type control at
		// all", which is not what anyone setting it asked for.
		if !spec.accepts(contentType) {
			return nil, fmt.Errorf("%w: %s declared as %s", ErrFileType, filename, contentType)
		}
	} else if !spec.acceptsDetected(detected) {
		return nil, fmt.Errorf("%w: %s is %s", ErrFileType, filename, detected)
	}

	tmp, err := os.CreateTemp("", "shuttle-upload-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tmp.Close() }()

	max := spec.maxSize()
	// One byte past the limit is enough to know it was exceeded, without
	// reading the rest of whatever was sent.
	body := io.MultiReader(bytes.NewReader(head), part)
	n, err := io.Copy(tmp, io.LimitReader(body, max+1))
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, err
	}
	if n > max {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrFileTooLarge, filename, max)
	}

	return &UploadedFile{
		Name:         filename,
		Size:         n,
		Type:         detected,
		DeclaredType: contentType,
		path:         tmp.Name(),
	}, nil
}

// acceptsDetected checks a sniffed type against Accept.
//
// It is accepts with one allowance: http.DetectContentType collapses every
// kind of text to text/plain, so a component accepting text/csv would
// otherwise refuse csv files - the exact case where the declared type was
// carrying the check.
func (u Upload) acceptsDetected(detected string) bool {
	if u.accepts(detected) {
		return true
	}
	got, _, err := mime.ParseMediaType(detected)
	if err != nil || got != "text/plain" {
		return false
	}
	for _, want := range u.Accept {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(want)), "text/") {
			return true
		}
	}
	return false
}

// discard removes received files. Called once the handler has had its
// chance at them.
func discard(files []*UploadedFile) {
	for _, f := range files {
		if f != nil && f.path != "" {
			// #nosec G703 -- path is a temp file this package created, never
			// anything the client named.
			_ = os.Remove(f.path)
		}
	}
}

// uploadID is the element id carrying an upload's progress.
func uploadID(p path, name string) string {
	return p.elementID() + "-upload-" + name
}

// FileInput wires a file input to one of the component's declared uploads.
//
//	input.New(
//	    shuttle.ID(ctx, input.ID, "avatar"),
//	    shuttle.FileInput(ctx, input.Attr, "avatar"),
//	)
//
// While an upload runs, shuttle's shim puts data-shuttle-uploading and
// data-shuttle-progress on the input, and sets a --shuttle-progress custom
// property, so a progress bar is a matter of CSS rather than a round trip
// per percent.
func FileInput[O ~func(T), T any](ctx context.Context, attr Attrs[O], name string) O {
	sc, ok := scopeFrom(ctx)
	if !ok {
		return func(T) {}
	}

	spec, ok := uploadFor(sc.node.cmp, name)
	if !ok {
		// Rendering a picker for an upload the component never declared
		// would post to an endpoint that refuses it. Better to render an
		// input that does nothing.
		return func(T) {}
	}

	// No session in the URL: the shim adds it as a header, the same way
	// every other request carries it. See [SessionHeader].
	sess := sc.node.sess
	endpoint := fmt.Sprintf("%s/upload/%s/%s",
		sess.prefix+routePrefix, sc.path().nodeID(), name)

	attrs := []pair{
		{"type", "file"},
		{"data-shuttle-upload", endpoint},
		{"id", uploadID(sc.path(), name)},
	}
	if len(spec.Accept) > 0 {
		attrs = append(attrs, pair{"accept", strings.Join(spec.Accept, ",")})
	}
	if spec.maxFiles() > 1 {
		attrs = append(attrs, pair{"multiple", ""})
	}
	return bind(attr, attrs...)
}
