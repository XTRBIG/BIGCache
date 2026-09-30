BIN     := bin/bigcache
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test race smoke bench fmt vet clean install

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/bigcache

test:
	go test -race -count=1 ./...

smoke:
	./scripts/smoke.sh

bench: build
	$(BIN) bench

fmt:
	gofmt -l -w .

vet:
	go vet ./...

install: build
	install -m 0755 $(BIN) /usr/local/bin/bigcache
	install -d /etc/bigcache
	[ -f /etc/bigcache/config.json ] || install -m 0600 examples/config.json /etc/bigcache/config.json
	install -m 0644 examples/bigcache.service /etc/systemd/system/bigcache.service
	install -m 0644 examples/attach@.service /etc/systemd/system/bigcache-attach@.service

clean:
	rm -rf bin
