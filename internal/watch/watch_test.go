package watch

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func start(t *testing.T) (root string, calls *atomic.Int32) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".zk"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls = &atomic.Int32{}
	w := &Watcher{
		Root: root, Exts: map[string]bool{".md": true}, Debounce: 80 * time.Millisecond,
		OnChange: func() { calls.Add(1) }, Log: log.New(io.Discard, "", 0),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	time.Sleep(100 * time.Millisecond) // let the initial watches register
	return root, calls
}

func write(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBurstIsDebounced(t *testing.T) {
	root, calls := start(t)
	for i := 0; i < 5; i++ {
		write(t, filepath.Join(root, "a.md"))
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestIgnoresHiddenAndOtherExtensions(t *testing.T) {
	root, calls := start(t)
	write(t, filepath.Join(root, ".zk", "notebook.db"))
	write(t, filepath.Join(root, "image.png"))
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Errorf("calls = %d, want 0", n)
	}
}

func TestNewSubdirectoryIsWatched(t *testing.T) {
	root, calls := start(t)
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	calls.Store(0)
	write(t, filepath.Join(sub, "b.md"))
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1 (file in new subdir)", n)
	}
}
