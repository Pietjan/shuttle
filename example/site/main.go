// Command site is the shuttle examples site: one focused example per
// feature, each with its source alongside it.
//
//	make run                                 # compiles the stylesheet first
//	make run/tls                             # over HTTP/2, for several open pages
//	go run ./example/site -css "" -addr localhost:8080
//
// Use -tls if you keep several examples open at once. A browser allows
// about six connections per origin over HTTP/1.1 and every live page holds
// one for its stream, so the sixth page simply never loads - no error, it
// just hangs. HTTP/2 multiplexes every stream onto one connection and the
// limit stops existing, and browsers only speak it over TLS.
//
// Loom's markup is Tailwind and compiling it is the app's job, so this site
// compiles its own: `make site/css` writes static/styles.css, and `make run`
// does it first. Two commands, because cmd/css writes the Tailwind entry
// file and points @source at Loom's module directory - so the sheet carries
// every class baked into Loom's components, not only the ones some other
// site happens to render.
//
// Borrowing Loom's own site build was the shortcut, and it does not work as
// well as it looks: that sheet redefines the dark: variant as class-based
// for the theme toggle Loom's site has and this one doesn't, so its
// components stay light whatever the page around them does.
//
// Every example is a separate shuttle.Handler mounted under its own path,
// which is also the simplest demonstration of Handler.Prefix: shuttle
// builds the URLs it renders from wherever it was mounted.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	cryptotls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/a-h/templ"

	"github.com/pietjan/loom/code"
	"github.com/pietjan/loom/icon"
	"github.com/pietjan/shuttle"
)

//go:embed *.go
var source embed.FS

// Example is one entry in the site.
type Example struct {
	Slug  string
	Title string
	Blurb string
	// File is the source shown beneath it.
	File string
	// Hint is an instruction the example needs to make sense, like opening
	// a second tab.
	Hint string
	// Icon is the navigation glyph. navlist styles icons in its items, so
	// this is what the list was built to carry.
	Icon icon.Name
	// Wide fills the column rather than shrink-wrapping. The demo section
	// hugs its content so a button is button-sized; a table wants the room.
	Wide bool
	New  func() shuttle.Component
}

func examples() []Example {
	return []Example{
		{
			Slug: "counter", Title: "Counter", File: "counter.go",
			Icon: icon.Plus,
			Blurb: "State in a struct field, an action as a closure over it. " +
				"Nothing is serialised to the client and nothing comes back.",
			New: func() shuttle.Component { return &Counter{} },
		},
		{
			Slug: "form", Title: "Form", File: "form.go",
			Icon: icon.At,
			Blurb: "Validate as you type, commit on submit. The typing lives on " +
				"the client; only the committed value reaches the server's state.",
			New: func() shuttle.Component { return &Signup{} },
		},
		{
			Slug: "indicator", Title: "Pending state", File: "indicator.go",
			Icon: icon.Spinner,
			Blurb: "A slow action says so without being asked. The signal is the " +
				"client's own, so the spinner costs no round trip.",
			Hint: "The save takes 1.2s of deliberately fake work.",
			New:  func() shuttle.Component { return &Saving{} },
		},
		{
			Slug: "navigation", Title: "Navigation", File: "navigation.go",
			Icon:  icon.Compass,
			Blurb: "State in the URL, so a view can be shared and the back button works.",
			Hint:  "Watch the address bar, then press back.",
			New:   func() shuttle.Component { return &Filters{} },
		},
		{
			Slug: "nesting", Title: "Nested components", File: "nesting.go",
			Icon: icon.TreeStructure,
			Blurb: "Keyed children with their own state and their own morph target. " +
				"Ticking one row re-renders that row alone.",
			New: func() shuttle.Component { return &Board{} },
		},
		{
			Slug: "realtime", Title: "Pub/sub and presence", File: "realtime.go",
			Icon: icon.Broadcast,
			Blurb: "The stream is already open, so a message from anywhere reaches " +
				"every connected page without the client asking.",
			Hint: "Open this page in a second tab and shout from one of them.",
			New:  func() shuttle.Component { return &Room{} },
		},
		{
			Slug: "stream", Title: "Streams", File: "stream.go",
			Icon: icon.ListDashes,
			Blurb: "Appending to a list the server never holds, so a page open all " +
				"day costs what one just opened does.",
			New: func() shuttle.Component { return &Log{} },
		},
		{
			Slug: "upload", Title: "File upload", File: "upload.go",
			Icon: icon.UploadSimple,
			Blurb: "Its own request, because the stream only goes one way and fetch " +
				"reports no upload progress at all. Then the server's half of the " +
				"progress, which is ordinary state coming back down the stream.",
			Hint: "Over localhost the transfer is instant; the storing is 1.5s of fake work.",
			New:  func() shuttle.Component { return &Files{} },
		},
		{
			Slug: "combobox", Title: "Combobox", File: "kit.go",
			Icon: icon.MagnifyingGlass,
			Blurb: "Filter-as-you-type over a set the server owns - the case Loom " +
				"lists as missing. Arrow keys move real focus, so Enter stays native.",
			Hint: "Type, then use the arrow keys and Escape.",
			New:  func() shuttle.Component { return newCombobox() },
		},
		{
			Slug: "table", Title: "Live table", File: "kit.go", Wide: true,
			Icon: icon.Table,
			Blurb: "Sort, filter, paginate and choose columns over a set the " +
				"component never holds more than one page of. The whole view - " +
				"which columns included - is in the URL.",
			New: func() shuttle.Component { return newTable() },
		},
	}
}

func main() {
	css := flag.String("css", "example/site/static/styles.css",
		"compiled stylesheet to serve; `make site/css` writes it, and \"\" serves none")
	addr := flag.String("addr", "localhost:8080", "listen address")
	// Over HTTP/1.1 a browser allows about six connections per origin, and
	// every live page holds one for its stream - so five or six open pages
	// and the next one will not load. HTTP/2 multiplexes them all onto one
	// connection and the limit stops existing, which browsers only speak
	// over TLS.
	tls := flag.Bool("tls", false, "serve over HTTPS/2 with a self-signed certificate")
	flag.Parse()

	all := examples()
	prerender(all)

	// One handler for every example, so the whole site is one session and
	// one stream. Nine handlers with href links between them would be nine
	// page loads and nine streams, and a browser only allows about six
	// connections per origin - which is exactly how this site used to stop
	// loading once enough examples had been opened.
	h := shuttle.New(func() shuttle.Component { return &Site{All: all} })
	h.Prefix = "/e"
	h.Subtree = true
	h.Title = "shuttle examples"

	mux := http.NewServeMux()
	mux.Handle("/e/", h)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/e/"+all[0].Slug+"/", http.StatusFound)
	})
	styled := *css != ""
	if styled {
		// Loudly, because the flag now defaults to a real path: a page that
		// looks wrong should say why rather than leave it to be guessed.
		if _, err := os.Stat(*css); err != nil {
			log.Printf("no stylesheet at %s - run `make site/css`; serving unstyled", *css)
			styled = false
		}
	}
	h.Shell = shell(styled)
	if styled {
		mux.HandleFunc("GET /styles.css", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, *css)
		})
	}

	if *tls {
		cert, err := selfSigned()
		if err != nil {
			log.Fatal(err)
		}
		srv := &http.Server{
			Addr:      *addr,
			Handler:   mux,
			TLSConfig: &cryptotls.Config{Certificates: []cryptotls.Certificate{cert}},
		}
		log.Printf("listening on https://%s (HTTP/2; the certificate is self-signed)", *addr)
		log.Fatal(srv.ListenAndServeTLS("", ""))
	}

	log.Printf("listening on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// selfSigned makes a throwaway certificate for localhost, so the site can
// be served over HTTP/2 without any setup. Browsers will not trust it -
// that is what makes it a demonstration rather than a deployment.
func selfSigned() (cryptotls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return cryptotls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"shuttle examples"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return cryptotls.Certificate{}, err
	}
	return cryptotls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// shell renders the page around one example: the navigation, the example
// itself, and its source.
//
// A Shell is handed everything it needs to build the document, and owes the
// document two things back - Page.Attach on <body>, and Page.Scripts
// somewhere - or the page will not connect and its back button will not
// work.
func shell(styled bool) shuttle.Shell {
	return func(w io.Writer, p shuttle.Page) error {
		stylesheet := ""
		if styled {
			stylesheet = `<link rel="stylesheet" href="/styles.css">`
		}

		// The shell is only the document now. Everything inside it - the
		// navigation included - is the component's, because that is what a
		// morph replaces when you move between examples.
		_, err := fmt.Fprintf(w, `<!doctype html>
<html lang="en" class="scheme-light dark:scheme-dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<script type="module" src="%s"></script>
%s
<style>%s</style>
%s</head>
<body class="bg-base-50 text-base-800 dark:bg-base-900 dark:text-base-100" data-init="%s">
<div class="hidden bg-amber-700 px-4 py-2 text-center text-sm text-white in-[.shuttle-dead]:block in-[.shuttle-reconnecting]:block">Connection lost. Trying to reconnect…</div>
%s
</body>
</html>
`,
			html.EscapeString(p.Title),
			html.EscapeString(p.ScriptURL),
			stylesheet,
			siteCSS,
			p.Scripts,
			html.EscapeString(p.Attach),
			p.Body,
		)
		return err
	}
}

// sourceOf returns an embedded example's source.
func sourceOf(file string) string {
	b, err := source.ReadFile(file)
	if err != nil {
		return "// could not read " + file + ": " + err.Error()
	}
	return string(b)
}

// blocks holds each example's source as Loom's code component, highlighted
// once.
//
// Once, because highlighting is not cheap and an embedded file cannot
// change: the source sits inside the page the chrome re-renders, so doing
// it per render would lex a couple of hundred lines every time anyone
// clicked anything. Before the first request, because after that this map
// is read from every session's own goroutine.
var blocks = map[string]templ.Component{}

func prerender(all []Example) {
	for _, ex := range all {
		if _, done := blocks[ex.File]; done {
			continue
		}
		var buf bytes.Buffer
		block := code.New(sourceOf(ex.File), code.Language("go"), code.Class("border-0"))
		if err := block.Render(context.Background(), &buf); err != nil {
			log.Fatalf("highlighting %s: %v", ex.File, err)
		}
		blocks[ex.File] = templ.Raw(buf.String())
	}
}

// sourceBlock returns the highlighted source, falling back to highlighting
// on the spot for a file prerender never saw.
func sourceBlock(file string) templ.Component {
	if block, ok := blocks[file]; ok {
		return block
	}
	return code.New(sourceOf(file), code.Language("go"))
}

// siteCSS is the page around the components, and only that. The navigation,
// the headings, the callouts and the source blocks are Loom's, which is the
// split Loom asks for: it styles components, not your document.
//
// What is left here has to be hand-written rather than Tailwind utilities.
// cmd/css points Tailwind's @source at Loom's module directory, so every
// class inside a Loom component is compiled - but nothing scans this site's
// own sources, so a utility written here would not be in the sheet at all.
// siteCSS is what could not become a class, and it is now one rule.
//
// The upload bar's fill is loom's progress-bar element, which carries an
// inline width - so this has to out-specify an inline style from outside the
// component, and !important is the honest way to say that. Everything else
// the site used to declare here is a Tailwind class on the element that
// needed it, because the entry file @sources this repository as well as
// loom.
const siteCSS = `
input[data-shuttle-uploading] ~ [data-ui="progress"] [data-ui="progress-bar"] {
    width: var(--shuttle-progress, 0%) !important;
    transition: width .1s linear;
}
`
