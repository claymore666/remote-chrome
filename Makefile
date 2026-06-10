.PHONY: build build-go build-ext test test-uat lint clean uat-chrome

build: build-go build-ext

build-go:
	go build -o bin/remote-chrome ./cmd/remote-chrome

# --include=dev: a NODE_ENV=production environment otherwise prunes
# devDependencies and esbuild vanishes mid-build (FINDINGS.md #8).
build-ext:
	cd extension && npm install --include=dev --no-audit --no-fund && npm run build

# Unit + module tests (no Chrome required).
test:
	go test ./internal/...

# End-to-end regression suite against real headless Chrome (Chrome for
# Testing; branded Chrome ignores --load-extension since 137).
test-uat:
	go test -tags uat ./test/uat -timeout 300s

# One-time: fetch Chrome for Testing for the UAT suite.
uat-chrome:
	npx -y @puppeteer/browsers install chrome@stable --path ~/.cache/remote-chrome-uat

lint:
	gofmt -l . && test -z "$$(gofmt -l .)"
	go vet ./...
	go vet -tags uat ./test/uat
	cd extension && npx tsc --noEmit

clean:
	rm -rf bin extension/dist
