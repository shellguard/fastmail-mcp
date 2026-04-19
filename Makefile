VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)
BINARY  := fastmail-mcp
DIST    := dist

.PHONY: build clean release install test vet

# ── Development ──────────────────────────────────────────────────────────────

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

vet:
	go vet ./...

test:
	go test -v ./...

clean:
	rm -rf $(BINARY) $(BINARY).exe $(DIST)

install: build
	./install.sh

# ── Release (all platforms) ──────────────────────────────────────────────────

PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	linux/arm64 \
	windows/amd64 \
	windows/arm64

release: clean
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		outdir="$(DIST)/$(BINARY)-$$os-$$arch"; \
		mkdir -p "$$outdir"; \
		echo "Building $$os/$$arch..."; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o "$$outdir/$(BINARY)$$ext" . || exit 1; \
		if [ "$$os" = "windows" ]; then \
			(cd $(DIST) && zip -q "$(BINARY)-$$os-$$arch.zip" -r "$(BINARY)-$$os-$$arch/"); \
		else \
			tar -czf "$(DIST)/$(BINARY)-$$os-$$arch.tar.gz" -C $(DIST) "$(BINARY)-$$os-$$arch"; \
		fi; \
		rm -rf "$$outdir"; \
	done
	@echo "Release archives in $(DIST)/"
	@ls -lh $(DIST)/

# ── Checksums ────────────────────────────────────────────────────────────────

checksums: release
	@cd $(DIST) && shasum -a 256 *.tar.gz *.zip > checksums.txt
	@cat $(DIST)/checksums.txt
