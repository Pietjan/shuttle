# Change these variables as necessary.
addr = localhost:8080
tls_addr = localhost:8443
example_package_path = ./example/site

# Loom's markup is Tailwind and compiling it is the app's job, so the
# examples site builds its own stylesheet rather than shipping one or
# borrowing Loom's. Borrowing was the shortcut: that build is Loom's site's,
# down to a class-based dark: variant its theme toggle needs, so the
# components came out light on whatever canvas this site painted. Generating
# it is two commands.
site_css = example/site/static/styles.css

# The browser for the end-to-end suite. The official image ships the browsers
# and system libraries but not the npm package, which it expects from the
# project under test - hence the npx line in test/e2e. This version has to
# match the driver playwright-go pins (run.go: playwrightCliVersion).
# No download-host override any more: the old driver-zip CDN answers 400
# for every version, and playwright-go >= v0.6201 assembles its driver
# from the playwright-core npm package instead.
playwright_version := 1.62.1

# The CLIs live in their own module so that the published library does not
# inherit their dependency trees. -modfile runs them from that module
# without changing the working directory, which is what lets a tool act on
# the module it is invoked from.
tools = -modfile=tools/go.mod

# ==================================================================================== #
# HELPERS
# ==================================================================================== #

## help: print this help message
.PHONY: help
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

.PHONY: confirm
confirm:
	@echo -n 'Are you sure? [y/N] ' && read ans && [ $${ans:-N} = y ]

.PHONY: no-dirty
no-dirty:
	@test -z "$(shell git status --porcelain)"

# ==================================================================================== #
# QUALITY CONTROL
# ==================================================================================== #

## audit: run quality control checks
.PHONY: audit
audit: test
	go mod tidy -diff
	go mod verify
	test -z "$(shell go tool $(tools) gofumpt -l .)"
	go vet ./...
	go tool $(tools) golangci-lint run ./...
	go tool $(tools) govulncheck ./...
	cd tools && go mod tidy -diff
	cd tools && go mod verify

## test: run all tests
# -race is not optional here. A session runs every piece of component work
# on one goroutine while the transport reads from others, so the interesting
# bugs in this package are all races.
.PHONY: test
test:
	go test -race -buildvcs ./...
	cd broker/nats && go test -race -buildvcs ./...

## test/e2e: run the browser end-to-end suite in the playwright container
# The tag keeps these out of `make test`, which stays fast and needs no
# Docker; without SHUTTLE_E2E_WS they skip rather than fail, so a bare
# `go test -tags e2e ./...` is harmless.
.PHONY: test/e2e
test/e2e: site/css
	docker run -d --rm --init --name shuttle-playwright -p 3000:3000 \
		--add-host=host.docker.internal:host-gateway \
		mcr.microsoft.com/playwright:v$(playwright_version)-noble \
		/bin/sh -c "npx -y playwright@$(playwright_version) run-server --port 3000 --host 0.0.0.0"
	SHUTTLE_E2E_WS=ws://127.0.0.1:3000/ \
		go test -tags e2e -v -count 1 ./e2e; \
	status=$$?; docker stop shuttle-playwright >/dev/null; exit $$status

## test/cover: run all tests and display coverage
.PHONY: test/cover
test/cover:
	go test -race -buildvcs -coverprofile=/tmp/coverage.out ./...
	go tool cover -html=/tmp/coverage.out

## test/loom: check Loom's own suites still pass untouched
# Shuttle works through Loom's public seams and modifies nothing, so a
# change here that breaks Loom is a change that broke the rule.
.PHONY: test/loom
test/loom:
	cd ../loom && go test ./...

## upgradeable: list direct dependencies that have upgrades available
.PHONY: upgradeable
upgradeable:
	@go tool $(tools) go-mod-upgrade

## upgradeable/tools: list tool dependencies that have upgrades available
.PHONY: upgradeable/tools
upgradeable/tools:
	@cd tools && go tool -modfile=go.mod go-mod-upgrade

# ==================================================================================== #
# DEVELOPMENT
# ==================================================================================== #

## tidy: tidy modfiles, modernize and format .go files
.PHONY: tidy
tidy:
	go mod tidy -v
	cd tools && go mod tidy -v
	go tool $(tools) modernize -test -fix ./...
	go tool $(tools) gofumpt -l -w .

## generate: regenerate Go code from .templ files
# The generated *_templ.go files are checked in, against the usual advice:
# the examples are part of the published module, so they must build for
# anyone who `go get`s it - and for CI - without the templ CLI.
.PHONY: generate
generate:
	go tool $(tools) templ generate -path ./example/templ

## site/css: compile the examples site's stylesheet
# cmd/css writes loom.css - Tailwind, loom's theme and its structural CSS,
# with an @source pointing at the loom this module's replace directive
# resolves to. input.css next to it is hand-written: it imports that and
# adds an @source for this repository, so the classes in the live kit and
# the examples compile too.
.PHONY: site/css
site/css:
	go run github.com/pietjan/loom/cmd/css -o example/site/css/loom.css
	tailwindcss -i example/site/css/input.css -o $(site_css) --minify

## run: run the examples site on $(addr)
.PHONY: run
run: site/css
	go run $(example_package_path) -addr $(addr)

## run/tls: run the examples site over HTTPS/2 on $(tls_addr)
# The limit that makes several open pages stop loading is an HTTP/1.1 one:
# a browser allows about six connections per origin and every live page
# holds one for its stream. Over HTTP/2 they all share a single connection.
.PHONY: run/tls
run/tls: site/css
	go run $(example_package_path) -addr $(tls_addr) -tls

## run/plain: run the examples site with no stylesheet, to see the markup bare
.PHONY: run/plain
run/plain:
	go run $(example_package_path) -addr $(addr) -css ""

## run/counter: run the minimal one-component example
.PHONY: run/counter
run/counter:
	go run ./example/counter

## run/templ: run the .templ-authored example
.PHONY: run/templ
run/templ:
	go run ./example/templ

# ==================================================================================== #
# OPERATIONS
# ==================================================================================== #

## push: audit, then push to the tracking branch
.PHONY: push
push: confirm audit no-dirty
	git push

## release: tag a version and push the tag
# Usage: make release version=v0.1.0
.PHONY: release
release: confirm audit no-dirty
	@test -n "$(version)" || (echo 'usage: make release version=vX.Y.Z' && exit 1)
	git tag -a $(version) -m $(version)
	git push origin $(version)
