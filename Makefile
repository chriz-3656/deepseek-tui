BINARY  := deepseek-tui
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet tidy run cross clean

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

tidy:
	go mod tidy

run:
	go run .

cross:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe .

clean:
	rm -rf bin dist
