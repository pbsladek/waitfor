BINARY := waitfor
PKG := ./cmd/waitfor
DOCKER_IMAGE ?= pwbsladek/waitfor
DOCKER_TAG ?= local
DOCKER_PLATFORMS ?= linux/amd64,linux/arm64
DHI_GO_IMAGE ?= dhi.io/golang:1.26-dev
DHI_RUNTIME_IMAGE ?= dhi.io/static:20250419
VERSION ?=

.PHONY: build build-linux build-arm build-darwin build-windows build-all docker-build docker-push docker-run test test-e2e test-integration test-integration-docker test-integration-k8s lint security coverage bench tag push-tag release-tag release release-snapshot clean

build:
	go build -o bin/$(BINARY) $(PKG)

build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/$(BINARY)-linux-amd64 $(PKG)

build-arm:
	GOOS=linux GOARCH=arm64 go build -o bin/$(BINARY)-linux-arm64 $(PKG)

build-darwin:
	GOOS=darwin GOARCH=amd64 go build -o bin/$(BINARY)-darwin-amd64 $(PKG)
	GOOS=darwin GOARCH=arm64 go build -o bin/$(BINARY)-darwin-arm64 $(PKG)

build-windows:
	GOOS=windows GOARCH=amd64 go build -o bin/$(BINARY)-windows-amd64.exe $(PKG)
	GOOS=windows GOARCH=arm64 go build -o bin/$(BINARY)-windows-arm64.exe $(PKG)

build-all: build-linux build-arm build-darwin build-windows

docker-build:
	docker buildx build --load --build-arg GO_IMAGE=$(DHI_GO_IMAGE) --build-arg RUNTIME_IMAGE=$(DHI_RUNTIME_IMAGE) -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

docker-push:
	docker buildx build --platform $(DOCKER_PLATFORMS) --push --build-arg GO_IMAGE=$(DHI_GO_IMAGE) --build-arg RUNTIME_IMAGE=$(DHI_RUNTIME_IMAGE) -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

docker-run:
	docker run --rm $(DOCKER_IMAGE):$(DOCKER_TAG) $(ARGS)

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

bench:
	go test ./internal/cli ./internal/output ./internal/runner ./internal/condition -run '^$$' -bench . -benchmem -count=10

tag:
	@test -n "$(VERSION)" || (echo "VERSION is required, for example: make tag VERSION=v0.1.0" && exit 1)
	@case "$(VERSION)" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "VERSION must look like v0.1.0"; exit 1;; esac
	@git diff --quiet || (echo "working tree has uncommitted changes; commit before tagging" && exit 1)
	@git diff --cached --quiet || (echo "index has staged changes; commit before tagging" && exit 1)
	git tag -a "$(VERSION)" -m "Release $(VERSION)"

push-tag:
	@test -n "$(VERSION)" || (echo "VERSION is required, for example: make push-tag VERSION=v0.1.0" && exit 1)
	git push origin "$(VERSION)"

release-tag: tag push-tag
	@echo "Pushed $(VERSION); GitHub Actions will create the GitHub release, notes, archives, and checksums."

release:
	goreleaser release --clean

release-snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist coverage.out coverage.txt coverage.html
