.PHONY: build install clean setup-lib

CGO_LDFLAGS ?= -L$(CURDIR)/lib
export CGO_LDFLAGS

build: setup-lib
	go build -tags "sqlite_fts5" -o obliscence .

install: setup-lib
	go install -tags "sqlite_fts5" .

# Download prebuilt libtokenizers if missing.
setup-lib:
	@if [ ! -f lib/libtokenizers.a ]; then \
		echo "downloading libtokenizers..."; \
		mkdir -p lib; \
		curl -sL "https://github.com/daulet/tokenizers/releases/latest/download/libtokenizers.$$(go env GOOS)-$$(go env GOARCH).tar.gz" | tar xz -C lib/; \
	fi

clean:
	rm -f obliscence
