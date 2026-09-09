# The build lives here. devbox is one way to put the tools on PATH; the manual
# install on another machine is another. Neither is called from this file, so
# `make` behaves identically under both.

include versions.mk

GO      ?= go
GREN    ?= gren
BIN     ?= bin/ai-reviewer
GOFLAGS ?=

# Formatting Gren is a separate tool from the compiler; `gren format` is not a
# compiler subcommand. It needs node >= 20, which devbox.json supplies as
# nodejs@22. Both targets below skip it rather than fail when it is absent, so a
# machine without node can still build and test.
GREN_FORMAT ?= gren-format
GREN_SRC    ?= web/src

# Toolchain downloads are disabled: go.mod pins a floor of $(GO_VERSION), and a
# silent switch to a different compiler is exactly the drift versions.mk exists
# to catch.
export GOTOOLCHAIN = local

.PHONY: all
all: web build

.PHONY: build
build: web
	$(GO) build $(GOFLAGS) -o $(BIN) ./cmd/ai-reviewer

# The Gren output is embedded into the binary, so a built ai-reviewer needs
# neither gren nor node on the machine that serves the review.
.PHONY: web
web:
	cd web && $(GREN) make Main --optimize --output=dist/app.js

.PHONY: web-debug
web-debug:
	cd web && $(GREN) make Main --output=dist/app.js

.PHONY: test
test:
	$(GO) test ./...

.PHONY: check
check: test
	$(GO) vet ./...
	@out=$$(gofmt -l cmd internal web); \
	  if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@if command -v $(GREN_FORMAT) >/dev/null 2>&1; then \
	  out=$$($(GREN_FORMAT) --diff -r $(GREN_SRC)); \
	  if [ -n "$$out" ]; then \
	    echo "$$out"; echo; echo "gren-format needed: run make fmt"; exit 1; \
	  fi; \
	else \
	  echo "skipping Gren format check: $(GREN_FORMAT) not on PATH"; \
	fi

.PHONY: fmt
fmt:
	gofmt -w cmd internal web
	@if command -v $(GREN_FORMAT) >/dev/null 2>&1; then \
	  $(GREN_FORMAT) -r $(GREN_SRC); \
	else \
	  echo "skipping Gren formatting: $(GREN_FORMAT) not on PATH"; \
	fi

.PHONY: run
run: build
	./$(BIN) serve --root ./docs

.PHONY: clean
clean:
	rm -rf bin web/dist/app.js web/.gren

# tools reports what is installed against what versions.mk expects, and says how
# to get each one either way.
.PHONY: tools
tools:
	@printf 'expected  go %s, gren %s, node %s (node only for gren-format)\n\n' \
	  '$(GO_VERSION)' '$(GREN_VERSION)' '$(NODE_VERSION)'
	@printf 'go    '; \
	  if command -v $(GO) >/dev/null 2>&1; then $(GO) version; \
	  else echo 'MISSING     devbox: devbox add go@$(GO_VERSION)   manual: https://go.dev/dl/'; fi
	@printf 'gren  '; \
	  if command -v $(GREN) >/dev/null 2>&1; then $(GREN) --version; \
	  else echo 'MISSING     devbox: devbox add gren@$(GREN_VERSION)   manual: npm i -g gren-lang@$(GREN_VERSION) (needs node >= 22)'; fi
	@printf 'git   '; \
	  if command -v git >/dev/null 2>&1; then git --version; else echo 'MISSING'; fi
	@printf 'node  '; \
	  if command -v node >/dev/null 2>&1; then node --version; \
	  else echo 'absent      devbox: devbox add nodejs@$(NODE_VERSION)   (only needed for gren-format)'; fi
	@printf 'fmt   '; \
	  if command -v $(GREN_FORMAT) >/dev/null 2>&1; then $(GREN_FORMAT) --version 2>/dev/null || echo present; \
	  else echo 'absent      devbox: provided by nodejs@22 + npm i -g gren-format   manual: needs node >= 20'; fi
	@printf 'claude '; \
	  if command -v claude >/dev/null 2>&1; then claude --version; \
	  else echo 'MISSING     https://claude.com/claude-code'; fi
