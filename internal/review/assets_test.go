package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// imageURLs walks a rendered document for the URLs its images point at.
func imageURLs(node mdast.Node) []string {
	var out []string
	if node.Kind == mdast.KindImage {
		out = append(out, node.URL)
	}
	for _, child := range node.Children {
		out = append(out, imageURLs(child)...)
	}
	return out
}

func renderOne(t *testing.T, rev *Review, docPath string) []string {
	t.Helper()
	doc, err := rev.Render(docPath)
	require.NoError(t, err, "Render")
	return imageURLs(doc.Root)
}

func TestImagesAreServedFromTheFileRoute(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "# Flow\n\n![the flow](flow.png)\n")
	write(t, filepath.Join(root, "flow.png"), "not really a png")

	rev := newReview(t, root)

	urls := renderOne(t, rev, "spec.md")
	require.Len(t, urls, 1, "want one image")
	assert.True(t, strings.HasPrefix(urls[0], "/file/flow.png?"), "url = %q, want it pointed at the file route", urls[0])
	assert.Contains(t, urls[0], "v=", "want a version stamp")
}

// The point of the stamp. A regenerated diagram has to arrive under a URL the
// browser has not seen, or the <img> already on the page is never re-fetched.
func TestARegeneratedImageGetsANewURL(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "![the flow](flow.png)\n")
	image := filepath.Join(root, "flow.png")
	write(t, image, "first")

	rev := newReview(t, root)
	before := renderOne(t, rev, "spec.md")

	// What `dot -Tpng` does a moment later.
	later := time.Now().Add(2 * time.Second)
	write(t, image, "second, and larger")
	require.NoError(t, os.Chtimes(image, later, later))

	after := renderOne(t, rev, "spec.md")
	assert.NotEqual(t, before[0], after[0], "the URL did not change when the image did")
}

// A document rendered before its image exists must still get a usable URL, and
// must pick up a stamp once the file arrives.
func TestAMissingImageIsStillRoutedAndPicksUpAStamp(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "![the flow](flow.png)\n")

	rev := newReview(t, root)

	missing := renderOne(t, rev, "spec.md")
	assert.Equal(t, "/file/flow.png", missing[0], "want the route with no stamp")

	write(t, filepath.Join(root, "flow.png"), "made by dot")
	arrived := renderOne(t, rev, "spec.md")
	assert.True(t, strings.HasPrefix(arrived[0], "/file/flow.png?v="), "url = %q, want a stamp once the file exists", arrived[0])
}

func TestOnlyRelativeImagesInsideTheRootAreRewritten(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), strings.Join([]string{
		"![remote](https://example.com/a.png)",
		"![rooted](/already/absolute.png)",
		"![escaping](../outside.png)",
		"![inline](data:image/png;base64,AAAA)",
	}, "\n\n")+"\n")

	rev := newReview(t, root)

	for _, url := range renderOne(t, rev, "spec.md") {
		assert.False(t, strings.HasPrefix(url, AssetRoute), "url %q was rewritten and should have been left alone", url)
	}
}

// An image beside a document in a subdirectory resolves against the document,
// not the review root.
func TestImagePathsResolveAgainstTheDocument(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "design"), 0o755))
	write(t, filepath.Join(root, "design", "spec.md"), "![](flow.png)\n")
	write(t, filepath.Join(root, "design", "flow.png"), "png")

	rev := newReview(t, root)

	urls := renderOne(t, rev, "design/spec.md")
	assert.True(t, strings.HasPrefix(urls[0], "/file/design/flow.png?"), "url = %q, want it resolved beside the document", urls[0])
}

// The index the watcher uses: a rendered document knows what it points at, so a
// change to that file can find its way back to the document.
func TestARenderedDocumentIsFoundByItsImage(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "![](flow.png)\n")
	write(t, filepath.Join(root, "flow.png"), "png")

	rev := newReview(t, root)

	assert.Empty(t, rev.docsUsing("flow.png"), "docsUsing before any render")
	renderOne(t, rev, "spec.md")

	assert.Equal(t, []string{"spec.md"}, rev.docsUsing("flow.png"), "want the document that embeds it")

	// The image is taken out of the document; it stops depending on it.
	write(t, filepath.Join(root, "spec.md"), "no diagram any more\n")
	renderOne(t, rev, "spec.md")
	assert.Empty(t, rev.docsUsing("flow.png"), "docsUsing after the image was removed")
}

func TestAssetPathRefusesWhatIsNotUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "hello\n")
	write(t, filepath.Join(root, "flow.png"), "png")
	rev := newReview(t, root)

	_, err := rev.AssetPath("flow.png")
	assert.NoError(t, err, "AssetPath refused a file in the root")
	for _, bad := range []string{"../outside.png", "../../etc/passwd", ".git/config", "sub/../../out.png"} {
		_, err := rev.AssetPath(bad)
		assert.Error(t, err, "AssetPath allowed %q", bad)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// The case this exists for: the model writes a Graphviz file and cannot run
// `dot`, so the reviewer runs it themselves. No Markdown changes, and the
// document on screen has to pick the diagram up anyway.
func TestARegeneratedImageRepublishesTheDocument(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "# Flow\n\n![the flow](flow.dot.png)\n")

	rev := newReview(t, root)
	frames, cancel := rev.Subscribe()
	defer cancel()

	// Only a document that has been rendered is known to embed anything, which
	// is what opening one in the browser does.
	require.NoError(t, rev.PublishDoc("spec.md"))

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = rev.Watch(ctx) }()
	waitForWatcher(t, frames, root)

	write(t, filepath.Join(root, "flow.dot.png"), "what dot just produced")

	url := firstImageURL(t, waitForDoc(t, frames, "spec.md"))
	assert.True(t, strings.HasPrefix(url, "/file/flow.dot.png?v="), "republished document points at %q, want the new image", url)
}

// waitForWatcher blocks until the watcher is delivering events. Watch sets
// fsnotify up in a goroutine, so a test that writes a file straight away is
// racing it — and the write it loses is the one the test is about.
//
// The probe is rewritten until its own render comes back, since there is no
// telling which attempt was the first the watcher saw.
func waitForWatcher(t *testing.T, frames <-chan []byte, root string) {
	t.Helper()

	probe := filepath.Join(root, "probe.md")
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		write(t, probe, "# probe\n")

		if frame := readFrame(frames, time.Second); frame != nil && isDoc(frame, "probe.md") {
			require.NoError(t, os.Remove(probe))
			return
		}
	}
	require.FailNow(t, "the watcher never delivered an event")
}

// waitForDoc returns the next render of one document, ignoring everything else
// on the way — including renders of other documents.
func waitForDoc(t *testing.T, frames <-chan []byte, docPath string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if frame := readFrame(frames, time.Second); frame != nil && isDoc(frame, docPath) {
			return frame
		}
	}
	require.FailNow(t, "no render arrived", "document %s", docPath)
	return nil
}

func readFrame(frames <-chan []byte, wait time.Duration) map[string]any {
	select {
	case data := <-frames:
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			return nil
		}
		return frame

	case <-time.After(wait):
		return nil
	}
}

func isDoc(frame map[string]any, docPath string) bool {
	if frame["type"] != "doc" {
		return false
	}
	doc, _ := frame["doc"].(map[string]any)
	return doc["path"] == docPath
}

func firstImageURL(t *testing.T, frame map[string]any) string {
	t.Helper()

	doc, _ := frame["doc"].(map[string]any)
	root, _ := doc["root"].(map[string]any)

	var walk func(node map[string]any) string
	walk = func(node map[string]any) string {
		if node["kind"] == "image" {
			url, _ := node["url"].(string)
			return url
		}
		children, _ := node["children"].([]any)
		for _, child := range children {
			if next, ok := child.(map[string]any); ok {
				if found := walk(next); found != "" {
					return found
				}
			}
		}
		return ""
	}

	url := walk(root)
	require.NotEmpty(t, url, "no image in the published document: %v", frame)
	return url
}
