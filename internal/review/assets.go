package review

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// AssetRoute is where the server hands out the files a document points at. It
// lives here because rendering is what writes these URLs; the server only
// answers them.
const AssetRoute = "/file/"

// linkAssets rewrites the image URLs in a rendered document and reports which
// files it now points at.
//
// Two things are being fixed at once. A document says `![](flow.png)`, which is
// relative to the document, and the browser is on a page whose URL says nothing
// about where the document lives — so the path has to be made absolute against
// the review root. And the URL is stamped with the file's modification time,
// which is what makes a regenerated diagram actually appear: an <img> whose src
// has not changed is not re-fetched, however many times the document around it
// is re-rendered.
//
// Only images are rewritten. A relative link to a file is a navigation, and
// answering it with a download is not obviously what the reviewer meant.
func (r *Review) linkAssets(docPath string, node *mdast.Node) []string {
	var used []string

	if node.Kind == mdast.KindImage {
		if rewritten, asset, ok := r.assetURL(docPath, node.URL); ok {
			node.URL = rewritten
			used = append(used, asset)
		}
	}
	for i := range node.Children {
		used = append(used, r.linkAssets(docPath, &node.Children[i])...)
	}
	return used
}

// assetURL rewrites one URL, returning the rewritten form and the review-root
// relative path it points at. Anything that is not a relative path into the
// review root — an absolute URL, a data: URI, a path climbing out of the root —
// is left exactly as the author wrote it.
func (r *Review) assetURL(docPath, raw string) (string, string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "", "", false
	}
	if parsed.Path == "" || strings.HasPrefix(parsed.Path, "/") {
		return "", "", false
	}

	asset := path.Join(path.Dir(docPath), parsed.Path)
	if asset == ".." || strings.HasPrefix(asset, "../") {
		return "", "", false
	}

	out := url.URL{Path: AssetRoute + asset, Fragment: parsed.Fragment}
	query := parsed.Query()
	if info, err := os.Stat(filepath.Join(r.root, filepath.FromSlash(asset))); err == nil && !info.IsDir() {
		query.Set("v", stamp(info))
	}
	out.RawQuery = query.Encode()

	return out.String(), asset, true
}

// stamp identifies one version of a file. Base 36 keeps a nanosecond timestamp
// down to a dozen characters, which is the difference between a URL a person
// can read in the network pane and one they cannot.
func stamp(info os.FileInfo) string {
	return strconv.FormatInt(info.ModTime().UnixNano(), 36)
}

// noteAssets records what a document's last render pointed at, so a change to
// one of those files can find its way back to the documents that show it.
func (r *Review) noteAssets(docPath string, used []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(used) == 0 {
		delete(r.assets, docPath)
		return
	}
	r.assets[docPath] = used
}

// docsUsing lists the rendered documents that embed a file.
//
// Only documents that have been rendered are known, which is the right set:
// a document nobody has opened has nothing on screen to go stale.
func (r *Review) docsUsing(asset string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string
	for docPath, used := range r.assets {
		for _, candidate := range used {
			if candidate == asset {
				out = append(out, docPath)
				break
			}
		}
	}
	return out
}

// AssetPath turns a path from an asset URL into an absolute one, refusing
// anything that leaves the review root.
//
// Dot-prefixed segments are refused as well. No document embeds `.git/config`,
// and the review root is the one directory this daemon hands out over the
// network.
func (r *Review) AssetPath(rel string) (string, error) {
	// Refused, not cleaned away: cleaning "../secret" into "secret" answers a
	// request nobody made. Rendering only ever writes paths that are already
	// clean, so a dot segment here came from somewhere else.
	for _, segment := range strings.Split(rel, "/") {
		if strings.HasPrefix(segment, ".") {
			return "", errOutsideRoot(rel)
		}
	}
	return r.resolvePath(rel)
}
