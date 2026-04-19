BINARY := waitfor
PKG := ./cmd/waitfor

.PHONY: build build-linux build-arm test test-e2e lint coverage release clean

build:
	go build -o bin/$(BINARY) $(PKG)

build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/$(BINARY)-linux-amd64 $(PKG)

build-arm:
	GOOS=linux GOARCH=arm64 go build -o bin/$(BINARY)-linux-arm64 $(PKG)

test:
	go test ./...

test-e2e:
	go test -v ./e2e/...

lint:
	golangci-lint run

coverage:
	go test -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "HTML report written to coverage.html"

release:
	goreleaser release --clean

clean:
	rm -rf bin dist coverage.out coverage.html
