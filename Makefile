VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD := CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: dev dist test

# dev puts a build next to the plugin launcher, which prefers it over downloads.
dev:
	GOARCH=amd64 $(GOBUILD) -o plugin/bin/linux-use-amd64 ./cmd/linux-use

dist:
	rm -rf dist && mkdir -p dist
	GOARCH=amd64 $(GOBUILD) -o dist/linux-use-linux-amd64 ./cmd/linux-use
	GOARCH=arm64 $(GOBUILD) -o dist/linux-use-linux-arm64 ./cmd/linux-use
	cd dist && sha256sum linux-use-linux-* > checksums.txt

test:
	GOOS=linux go vet ./...
	go test ./...
