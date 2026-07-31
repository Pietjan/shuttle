package main

import (
	"context"
	"fmt"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/description"
	"github.com/pietjan/loom/fileupload"
	"github.com/pietjan/loom/progress"
	"github.com/pietjan/loom/text"
	"github.com/pietjan/shuttle"
)

// Files accepts uploads. They travel on their own request, because the
// stream only goes one way and cannot carry bytes up it - and they are
// driven by XMLHttpRequest, because fetch reports no upload progress at
// all.
//
// It shows the two halves of progress a real upload has, because they come
// from opposite directions:
//
//   - the transfer, which only the browser can see. It arrives as attributes
//     on the input and a --shuttle-progress custom property, so the bar is
//     CSS and costs no round trip and no server state. Over localhost it is
//     also over before you see it.
//   - what the server does with the bytes afterwards, which only the server
//     can see. That is state, it re-renders, and it travels down the stream
//     like everything else.
//
// The second one here is fake - a sleep pretending to be storage - but the
// machinery under it is not.
type Files struct {
	shuttle.Base
	Got []stored

	// Storing is what the server is pretending to write, and Percent how far
	// it claims to have got. Both are ordinary state: the bar moves because
	// the component re-renders, not because the client was told anything.
	Storing string
	Percent int
}

// Uploads declares what is accepted. These rules reach the file picker as
// hints and are enforced again on the server, where it matters: a client's
// copy of the rules is trivially skipped.
func (f *Files) Uploads() []shuttle.Upload {
	return []shuttle.Upload{{
		Name:     "docs",
		MaxSize:  2 << 20,
		MaxFiles: 3,
		Accept:   []string{"text/*", "image/*"},
	}}
}

// HandleUpload runs once the files have arrived and been checked. They are
// deleted when it returns, so anything worth keeping has to be copied out -
// UploadedFile.Save does that, and cleans the client's filename first.
func (f *Files) HandleUpload(_ context.Context, _ string, files []*shuttle.UploadedFile) error {
	arrived := make([]stored, 0, len(files))
	for _, file := range files {
		arrived = append(arrived, stored{file.Name, file.Type, file.Size})
	}

	f.Storing, f.Percent = describe(files), 0
	go f.store(arrived)
	return nil
}

// stored is what the component keeps once a file is gone: the temporary one
// is deleted the moment HandleUpload returns.
type stored struct {
	Name string
	Type string
	Size int64
}

// store is the fake work, and it runs on its own goroutine for a real
// reason: HandleUpload is on the session's, which every component on the
// page shares, so sleeping there would stall all of them. From outside that
// goroutine the only safe way in is Do, which queues the mutation and
// re-renders after it - a bare write from here would race the session.
//
// Do fails once the component is unmounted, which is the loop's exit when
// someone navigates away mid-store.
func (f *Files) store(arrived []stored) {
	for percent := 0; percent <= 100; percent += 4 {
		time.Sleep(60 * time.Millisecond)

		err := f.Do(func(context.Context) error {
			f.Percent = percent
			if percent == 100 {
				// Only now is it "stored", which is the honest moment to
				// show it - and the bar goes back to empty rather than
				// sitting full over a line that no longer mentions it.
				f.Got = append(f.Got, arrived...)
				f.Storing, f.Percent = "", 0
			}
			return nil
		})
		if err != nil {
			return
		}
	}
}

func describe(files []*shuttle.UploadedFile) string {
	if len(files) == 1 {
		return files[0].Name
	}
	return fmt.Sprintf("%d files", len(files))
}

func (f *Files) Render(ctx context.Context) templ.Component {
	// Loom's own file input, which styles the button the browser draws
	// (::file-selector-button) - the accept and multiple attributes come from
	// the Uploads spec above rather than being written twice.
	picker := fileupload.New(
		shuttle.ID(ctx, fileupload.ID, "docs"),
		shuttle.FileInput(ctx, fileupload.Attr, "docs"),
	)

	// One bar for both halves, which the CSS already arbitrates: while the
	// input carries data-shuttle-uploading the custom property wins, and the
	// rest of the time this value does. See siteCSS in main.go.
	bar := progress.New(progress.Class("w-full max-w-80"),
		progress.Value(float64(f.Percent)))

	status := "Text and images, up to 2 MiB and three at a time."
	if f.Storing != "" {
		status = fmt.Sprintf("Storing %s — %d%%", f.Storing, f.Percent)
	}

	parts := []templ.Component{
		picker,
		bar,
		inside(text.New(text.Subtle, text.Small), label(status)),
	}

	// What arrived is a set of facts about each file, which is a description
	// list rather than a bulleted one.
	if len(f.Got) > 0 {
		rows := make([]templ.Component, 0, len(f.Got)*2)
		for _, file := range f.Got {
			rows = append(rows,
				inside(description.Term(), label(file.Name)),
				inside(description.Detail(),
					label(fmt.Sprintf("%s · %d bytes", file.Type, file.Size))),
			)
		}
		parts = append(parts, inside(description.New(), rows...))
	}
	// .rows spaces the pieces; a component's own internals get no gap from
	// the demo section around it.
	return el(`<div class="grid gap-2">`, `</div>`, parts...)
}
