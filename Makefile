.PHONY: build install uninstall test test-neovim dist clean FORCE

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX ?= $(HOME)/.local
LIBDIR = $(PREFIX)/lib/trailmix
# Space-separated macOS architectures; dist builds a universal binary.
ARCHS ?= $(shell uname -m)

CAPTURE_APP = bin/TrailMixCapture.app
CAPTURE = $(CAPTURE_APP)/Contents/MacOS/trailmix-capture
DIST = dist/trailmix-$(VERSION)-darwin-universal
goarch = $(if $(filter x86_64,$(1)),amd64,$(1))

build: bin/trailmix $(CAPTURE)

bin/trailmix: FORCE
	mkdir -p bin .build
	$(foreach a,$(ARCHS),GOOS=darwin GOARCH=$(call goarch,$(a)) CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o .build/trailmix-$(a) ./cmd/trailmix &&) true
	lipo -create $(ARCHS:%=.build/trailmix-%) -output $@

# The CLI finds the helper beside its resolved executable path.
$(CAPTURE): native/Capture.swift native/Info.plist
	rm -rf $(CAPTURE_APP)
	mkdir -p $(CAPTURE_APP)/Contents/MacOS .build/swift-cache
	cp native/Info.plist $(CAPTURE_APP)/Contents/Info.plist
	plutil -replace CFBundleShortVersionString -string '$(VERSION)' $(CAPTURE_APP)/Contents/Info.plist
	$(foreach a,$(ARCHS),xcrun swiftc -swift-version 6 -O -parse-as-library -target $(a)-apple-macosx15.0 -module-cache-path .build/swift-cache native/Capture.swift -o .build/trailmix-capture-$(a) &&) true
	lipo -create $(ARCHS:%=.build/trailmix-capture-%) -output $@
	codesign --force --sign - $(CAPTURE_APP)

# Installs a self-contained copy, so the checkout can move or be rebuilt.
install: build
	rm -rf $(LIBDIR)
	mkdir -p $(LIBDIR) $(PREFIX)/bin
	cp bin/trailmix $(LIBDIR)/trailmix
	ditto $(CAPTURE_APP) $(LIBDIR)/TrailMixCapture.app
	ln -sf $(LIBDIR)/trailmix $(PREFIX)/bin/trailmix

uninstall:
	rm -f $(PREFIX)/bin/trailmix
	rm -rf $(LIBDIR)

dist: clean
	$(MAKE) build ARCHS="arm64 x86_64" VERSION=$(VERSION)
	mkdir -p $(DIST)
	cp bin/trailmix LICENSE README.md $(DIST)/
	ditto $(CAPTURE_APP) $(DIST)/TrailMixCapture.app
	tar -C dist -czf $(DIST).tar.gz $(notdir $(DIST))
	cd dist && shasum -a 256 $(notdir $(DIST)).tar.gz > $(notdir $(DIST)).tar.gz.sha256

clean:
	rm -rf bin dist .build/trailmix-*

test: build
	go test -race ./...
	$(CAPTURE) self-test
	$(MAKE) test-neovim

test-neovim:
	nvim --headless -u NONE --noplugin -i NONE -l tests/nvim/timestamp.lua
	nvim --headless -u NONE --noplugin -i NONE -l tests/nvim/transcript.lua
	nvim --headless -u NONE --noplugin -i NONE -l tests/nvim/ask.lua
