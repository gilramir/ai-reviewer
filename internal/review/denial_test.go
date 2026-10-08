package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Whether a refused path was out of reach decides which fix the browser
// suggests, so it has to survive the spellings a model uses: relative paths,
// globs, a file not yet written, and a symlink into the workspace.
func TestWhatIsOutOfReach(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "repo")
	extra := filepath.Join(base, "shared")
	elsewhere := filepath.Join(base, "elsewhere")
	for _, dir := range []string{work, extra, elsewhere} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(work, link))
	roots := []string{work, extra}

	for target, want := range map[string]bool{
		"docs/spec.md":                           true,
		"**/*.md":                                true,
		filepath.Join(work, "new/file.md"):       true,
		filepath.Join(extra, "notes.md"):         true,
		filepath.Join(link, "spec.md"):           true,
		filepath.Join(elsewhere, "notes.md"):     false,
		filepath.Join(elsewhere, "*.md"):         false,
		"../elsewhere/notes.md":                  false,
		filepath.Join(base, "repo-sibling/x.md"): false,
	} {
		assert.Equal(t, want, withinAny(work, target, roots), "withinAny(%q)", target)
	}
}
