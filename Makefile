BIN     := bin/bigcache
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test race smoke bench fmt vet clean install uninstall purge release

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

# Detaches, stops and disables the services after writing dirty blocks back,
# then removes the program. Configuration and the cache device are kept.
uninstall:
	/usr/local/bin/bigcache teardown -c /etc/bigcache/config.json
	rm -f /etc/systemd/system/bigcache.service /etc/systemd/system/bigcache-attach@.service
	systemctl daemon-reload 2>/dev/null || true
	rm -f /usr/local/bin/bigcache

# Like uninstall, but also deletes the cache file (or wipes the cache
# partition signature), the configuration and the log.
purge:
	/usr/local/bin/bigcache teardown -c /etc/bigcache/config.json --purge-cache --purge-config
	rm -f /etc/systemd/system/bigcache.service /etc/systemd/system/bigcache-attach@.service
	systemctl daemon-reload 2>/dev/null || true
	rm -f /usr/local/bin/bigcache

release:
	./scripts/build-release.sh $(VERSION)

clean:
	rm -rf bin dist
