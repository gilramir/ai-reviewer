# Single source of truth for toolchain versions.
# devbox.json pins the same versions; `make tools` checks reality against these.
# Go is pinned at minor precision: go.mod enforces a floor of 1.25.0 and
# GOTOOLCHAIN=local forbids switching, but nix resolves go@1.25 to whatever
# patch it carries. Naming a patch here would be a number nothing enforces.
GO_VERSION   := 1.25
GREN_VERSION := 0.6.6

# Node is not needed to build or run ai-reviewer. It exists only so gren-format
# can run, which needs >= 20; devbox supplies it as nodejs@22.
NODE_VERSION := 22
