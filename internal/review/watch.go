package review

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch pushes a fresh render to every browser whenever a document changes on
// disk.
//
// This is the whole re-render path. The daemon does not need to know whether a
// change came from Claude editing a file, the reviewer's own editor, or a git
// operation: the file changed, so the browser gets the new tree.
func (r *Review) Watch(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := r.watchTree(watcher); err != nil {
		return err
	}

	// Editors write a document as several operations — a rename, a truncate, a
	// write — and each one is an event. Waiting for the flurry to stop avoids
	// re-rendering a file mid-save, when it is briefly empty or truncated.
	const settle = 120 * time.Millisecond
	pending := map[string]bool{}
	var timer <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if !skipDir(filepath.Base(event.Name)) {
						_ = watcher.Add(event.Name)
					}
					r.PublishDocList()
					continue
				}
			}

			if !isMarkdown(event.Name) {
				continue
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				r.PublishDocList()
			}
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				// The conversation about a file that no longer exists refers to
				// a path that means nothing now.
				if rel, err := filepath.Rel(r.root, event.Name); err == nil {
					r.procs.Forget(filepath.ToSlash(rel))
				}
				continue
			}

			rel, err := filepath.Rel(r.root, event.Name)
			if err != nil {
				continue
			}
			pending[filepath.ToSlash(rel)] = true
			timer = time.After(settle)

		case <-timer:
			timer = nil
			for docPath := range pending {
				delete(pending, docPath)
				r.refresh(docPath)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			r.PublishError("watcher: " + err.Error())
		}
	}
}

// refresh re-reads a document, re-locates the threads on it, and pushes both.
func (r *Review) refresh(docPath string) {
	src, err := r.read(docPath)
	if err != nil {
		// The file was deleted between the event and this read; the document
		// list has already been republished.
		return
	}

	r.reanchor(docPath, string(src))

	if err := r.PublishDoc(docPath); err != nil {
		r.PublishError(err.Error())
		return
	}
	r.broadcastThreads(docPath)
	_ = r.save()
}

// watchTree adds the review root and every directory under it.
func (r *Review) watchTree(watcher *fsnotify.Watcher) error {
	return filepath.Walk(r.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if path != r.root && skipDir(info.Name()) {
			return filepath.SkipDir
		}
		return watcher.Add(path)
	})
}
