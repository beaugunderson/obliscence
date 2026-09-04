.PHONY: build install test clean setup-lib

CGO_LDFLAGS ?= -L$(CURDIR)/lib -Wl,-no_warn_duplicate_libraries
CGO_CFLAGS ?= -Wno-deprecated-declarations
export CGO_LDFLAGS CGO_CFLAGS

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
LDFLAGS := -X main.version=$(VERSION)

build: setup-lib
	go build -tags "sqlite_fts5" -ldflags "$(LDFLAGS)" -o obliscence .

install: setup-lib
	go install -tags "sqlite_fts5" -ldflags "$(LDFLAGS)" .

test: setup-lib
	go test -tags "sqlite_fts5" ./...

# Download prebuilt libtokenizers if missing.
setup-lib:
	@if [ ! -f lib/libtokenizers.a ]; then \
		echo "downloading libtokenizers..."; \
		mkdir -p lib; \
		curl -sL "https://github.com/daulet/tokenizers/releases/latest/download/libtokenizers.$$(go env GOOS)-$$(go env GOARCH).tar.gz" | tar xz -C lib/; \
	fi

clean:
	rm -f obliscence
