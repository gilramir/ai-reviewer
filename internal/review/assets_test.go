package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return imageURLs(doc.Root)
}

func TestImagesAreServedFromTheFileRoute(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "# Flow\n\n![the flow](flow.png)\n")
	write(t, filepath.Join(root, "flow.png"), "not really a png")

	rev := newReview(t, root)

	urls := renderOne(t, rev, "spec.md")
	if len(urls) != 1 {
		t.Fatalf("urls = %v, want one image", urls)
	}
	if !strings.HasPrefix(urls[0], "/file/flow.png?") {
		t.Errorf("url = %q, want it pointed at the file route", urls[0])
	}
	if !strings.Contains(urls[0], "v=") {
		t.Errorf("url = %q, want a version stamp", urls[0])
	}
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
	if err := os.Chtimes(image, later, later); err != nil {
		t.Fatal(err)
	}

	after := renderOne(t, rev, "spec.md")
	if before[0] == after[0] {
		t.Errorf("the URL did not change when the image did: %q", after[0])
	}
}

// A document rendered before its image exists must still get a usable URL, and
// must pick up a stamp once the file arrives.
func TestAMissingImageIsStillRoutedAndPicksUpAStamp(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "![the flow](flow.png)\n")

	rev := newReview(t, root)

	missing := renderOne(t, rev, "spec.md")
	if missing[0] != "/file/flow.png" {
		t.Errorf("url = %q, want the route with no stamp", missing[0])
	}

	write(t, filepath.Join(root, "flow.png"), "made by dot")
	arrived := renderOne(t, rev, "spec.md")
	if !strings.HasPrefix(arrived[0], "/file/flow.png?v=") {
		t.Errorf("url = %q, want a stamp once the file exists", arrived[0])
	}
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
		if strings.HasPrefix(url, AssetRoute) {
			t.Errorf("url %q was rewritten and should have been left alone", url)
		}
	}
}

// An image beside a document in a subdirectory resolves against the document,
// not the review root.
func TestImagePathsResolveAgainstTheDocument(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "design"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "design", "spec.md"), "![](flow.png)\n")
	write(t, filepath.Join(root, "design", "flow.png"), "png")

	rev := newReview(t, root)

	urls := renderOne(t, rev, "design/spec.md")
	if !strings.HasPrefix(urls[0], "/file/design/flow.png?") {
		t.Errorf("url = %q, want it resolved beside the document", urls[0])
	}
}

// The index the watcher uses: a rendered document knows what it points at, so a
// change to that file can find its way back to the document.
func TestARenderedDocumentIsFoundByItsImage(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "![](flow.png)\n")
	write(t, filepath.Join(root, "flow.png"), "png")

	rev := newReview(t, root)

	if docs := rev.docsUsing("flow.png"); len(docs) != 0 {
		t.Errorf("docsUsing before any render = %v, want none", docs)
	}
	renderOne(t, rev, "spec.md")

	docs := rev.docsUsing("flow.png")
	if len(docs) != 1 || docs[0] != "spec.md" {
		t.Errorf("docsUsing = %v, want the document that embeds it", docs)
	}

	// The image is taken out of the document; it stops depending on it.
	write(t, filepath.Join(root, "spec.md"), "no diagram any more\n")
	renderOne(t, rev, "spec.md")
	if docs := rev.docsUsing("flow.png"); len(docs) != 0 {
		t.Errorf("docsUsing after the image was removed = %v, want none", docs)
	}
}

func TestAssetPathRefusesWhatIsNotUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "spec.md"), "hello\n")
	write(t, filepath.Join(root, "flow.png"), "png")
	rev := newReview(t, root)

	if _, err := rev.AssetPath("flow.png"); err != nil {
		t.Errorf("AssetPath refused a file in the root: %v", err)
	}
	for _, bad := range []string{"../outside.png", "../../etc/passwd", ".git/config", "sub/../../out.png"} {
		if _, err := rev.AssetPath(bad); err == nil {
			t.Errorf("AssetPath allowed %q", bad)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if err := rev.PublishDoc("spec.md"); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = rev.Watch(ctx) }()
	waitForWatcher(t, frames, root)

	write(t, filepath.Join(root, "flow.dot.png"), "what dot just produced")

	url := firstImageURL(t, waitForDoc(t, frames, "spec.md"))
	if !strings.HasPrefix(url, "/file/flow.dot.png?v=") {
		t.Errorf("republished document points at %q, want the new image", url)
	}
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
			if err := os.Remove(probe); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("the watcher never delivered an event")
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
	t.Fatalf("no render of %s arrived", docPath)
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
	if url == "" {
		t.Fatalf("no image in the published document: %v", frame)
	}
	return url
}
