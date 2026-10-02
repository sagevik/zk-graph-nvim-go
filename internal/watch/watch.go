// Package watch reports (debounced) changes to note files below a notebook root.
package watch

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher calls OnChange once per burst of note-file changes.
type Watcher struct {
	Root     string
	Exts     map[string]bool // lower-case, with dot: ".md"
	Debounce time.Duration
	OnChange func()
	Log      *log.Logger
}

// hidden reports whether any path component below root starts with "." (.zk, .git, ...).
// Changes there must be ignored: zk writes .zk/notebook.db on every index, which would
// otherwise trigger a reload loop.
func (w *Watcher) hidden(path string) bool {
	rel, err := filepath.Rel(w.Root, path)
	if err != nil || rel == "." {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// relevant: a note file, or an extension-less path on remove/rename (a directory full of
// notes may have gone away; its entries produce no events of their own).
func (w *Watcher) relevant(ev fsnotify.Event) bool {
	ext := strings.ToLower(filepath.Ext(ev.Name))
	if w.Exts[ext] {
		return true
	}
	return ext == "" && ev.Has(fsnotify.Remove|fsnotify.Rename)
}

// Run blocks until ctx is done. inotify is not recursive, so every directory is watched
// individually and new directories are added as they appear.
func (w *Watcher) Run(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()

	addTree := func(dir string) {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if w.hidden(p) {
				return filepath.SkipDir
			}
			if err := fw.Add(p); err != nil {
				w.Log.Printf("watch %s: %v", p, err) // e.g. fs.inotify.max_user_watches reached
			}
			return nil
		})
	}
	addTree(w.Root)

	var mu sync.Mutex
	var timer *time.Timer
	schedule := func() {
		mu.Lock()
		defer mu.Unlock()
		if timer == nil {
			timer = time.AfterFunc(w.Debounce, w.OnChange)
		} else {
			timer.Reset(w.Debounce)
		}
	}
	defer func() {
		mu.Lock()
		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			if w.hidden(ev.Name) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					addTree(ev.Name) // may already contain notes (mv dir in, git checkout)
					schedule()
					continue
				}
			}
			if w.relevant(ev) {
				schedule()
			}
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.Log.Printf("watch: %v", err)
		}
	}
}
