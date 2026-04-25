BINARY := waitfor
PKG := ./cmd/waitfor

.PHONY: build build-linux build-arm test test-e2e test-integration test-integration-docker test-integration-k8s lint security coverage release clean

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

test-integration:
	WAITFOR_BLACKBOX=1 go test -count=1 -v ./integration/...

test-integration-docker:
	WAITFOR_BLACKBOX=1 WAITFOR_BLACKBOX_DOCKER=1 go test -count=1 -v ./integration/...

test-integration-k8s:
	WAITFOR_BLACKBOX=1 WAITFOR_BLACKBOX_K8S=1 go test -count=1 -v ./integration/...

lint:
	golangci-lint run ./...

security:
	golangci-lint run --enable=gosec ./...

coverage:
	go test -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tee coverage.txt
	awk '/^total:/ { sub(/%/, "", $$3); if ($$3 < 90) { printf("coverage %.1f%% is below 90%%\n", $$3); exit 1 } }' coverage.txt
	go tool cover -html=coverage.out -o coverage.html
	@echo "HTML report written to coverage.html"

release:
	goreleaser release --clean

clean:
	rm -rf bin dist coverage.out coverage.txt coverage.html
