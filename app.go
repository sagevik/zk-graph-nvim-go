package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"zk-graph/internal/graph"
	"zk-graph/internal/watch"
	"zk-graph/internal/zk"
)

// Options are fixed for the lifetime of the process (one process per notebook).
type Options struct {
	Root     string
	ZkBin    string
	ZkArgs   []string
	Config   json.RawMessage // opaque to Go: forwarded to the frontend (theme, filter, ...)
	Exts     map[string]bool
	Debounce time.Duration
}

// App owns the current graph and bridges three parties:
//   - nvim, over stdin/stdout (newline-delimited JSON, see inMsg/outMsg)
//   - the webview, via Wails bindings (Init, Open) and events (graph:*, note:current)
//   - the filesystem, via the watcher, which triggers `zk graph` reloads
type App struct {
	opts Options
	log  *log.Logger
	ctx  context.Context

	mu      sync.Mutex
	graph   *graph.Graph
	version int
	loadErr string
	current string // notebook-relative id of nvim's current buffer, "" if none

	outMu sync.Mutex
	out   io.Writer

	reload chan struct{}
}

// inMsg: nvim -> app.
type inMsg struct {
	Type string `json:"type"` // show | hide | current | refresh | quit
	ID   string `json:"id,omitempty"`
}

// outMsg: app -> nvim.
type outMsg struct {
	Type    string `json:"type"` // ready | open | error
	Path    string `json:"path,omitempty"`
	Message string `json:"message,omitempty"`
	Notes   int    `json:"notes,omitempty"`
}

func NewApp(opts Options, logger *log.Logger) *App {
	return &App{opts: opts, log: logger, out: os.Stdout, reload: make(chan struct{}, 1)}
}

func (a *App) send(m outMsg) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	a.outMu.Lock()
	defer a.outMu.Unlock()
	_, _ = a.out.Write(append(b, '\n'))
}

// requestReload coalesces: while a reload runs, at most one more is queued.
func (a *App) requestReload() {
	select {
	case a.reload <- struct{}{}:
	default:
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.reloadLoop()
	go a.readStdin()
	w := &watch.Watcher{
		Root: a.opts.Root, Exts: a.opts.Exts, Debounce: a.opts.Debounce,
		OnChange: a.requestReload, Log: a.log,
	}
	go func() {
		if err := w.Run(ctx); err != nil {
			a.log.Printf("watcher disabled: %v", err)
			a.send(outMsg{Type: "error", Message: "file watcher disabled: " + err.Error()})
		}
	}()
	a.requestReload()
}

func (a *App) reloadLoop() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.reload:
			a.doReload()
		}
	}
}

func (a *App) setError(err error) {
	a.log.Print(err)
	a.mu.Lock()
	a.loadErr = err.Error()
	a.mu.Unlock()
	runtime.EventsEmit(a.ctx, "graph:error", err.Error())
	a.send(outMsg{Type: "error", Message: err.Error()})
}

func (a *App) doReload() {
	start := time.Now()
	data, err := zk.Graph(a.ctx, a.opts.ZkBin, a.opts.Root, a.opts.ZkArgs)
	if err != nil {
		if a.ctx.Err() == nil {
			a.setError(err)
		}
		return
	}
	g, err := graph.Parse(data, a.opts.Root)
	if err != nil {
		a.setError(err)
		return
	}

	a.mu.Lock()
	first := a.graph == nil
	d := graph.Compute(a.graph, g, a.version)
	changed := first || !d.Empty()
	if changed {
		a.graph, a.version = g, d.Version
	}
	hadErr := a.loadErr != ""
	a.loadErr = ""
	a.mu.Unlock()

	if hadErr {
		runtime.EventsEmit(a.ctx, "graph:error", "")
	}
	switch {
	case first:
		// the frontend may not be listening yet; it pulls the snapshot via Init()
		runtime.EventsEmit(a.ctx, "graph:reset")
		a.send(outMsg{Type: "ready", Notes: len(g.Nodes)})
	case changed:
		runtime.EventsEmit(a.ctx, "graph:diff", d)
	}
	a.log.Printf("reload: %d nodes, %d edges, changed=%v, %s",
		len(g.Nodes), len(g.Edges), changed, time.Since(start).Round(time.Millisecond))
}

func (a *App) readStdin() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var m inMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			a.log.Printf("stdin: bad message %q: %v", sc.Text(), err)
			continue
		}
		switch m.Type {
		case "show":
			runtime.WindowShow(a.ctx)
			runtime.WindowUnminimise(a.ctx)
			// a window started hidden laid out with a 0x0 canvas: let the view re-fit
			runtime.EventsEmit(a.ctx, "window:show")
		case "hide":
			runtime.WindowHide(a.ctx)
		case "current":
			a.mu.Lock()
			a.current = m.ID
			a.mu.Unlock()
			runtime.EventsEmit(a.ctx, "note:current", m.ID)
		case "refresh":
			a.requestReload()
		case "quit":
			runtime.Quit(a.ctx)
			return
		default:
			a.log.Printf("stdin: unknown message type %q", m.Type)
		}
	}
	// EOF: nvim exited or crashed. Without this the process would outlive its editor.
	runtime.Quit(a.ctx)
}

// ---- bound to the frontend (window.go.main.App.*) --------------------------------

// InitPayload is everything the frontend needs on (re)load.
type InitPayload struct {
	Config  json.RawMessage `json:"config"`
	Root    string          `json:"root"`
	Graph   *graph.Snapshot `json:"graph"` // nil until the first load finished
	Error   string          `json:"error"`
	Current string          `json:"current"`
}

// Init returns the current snapshot. The frontend calls it at startup, on graph:reset,
// and whenever a diff does not apply to its version (missed events).
func (a *App) Init() InitPayload {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := InitPayload{Config: a.opts.Config, Root: a.opts.Root, Error: a.loadErr, Current: a.current}
	if a.graph != nil {
		s := a.graph.Snapshot(a.version)
		p.Graph = &s
	}
	return p
}

// Log writes a frontend diagnostic to the process log (:ZkGraphLog in nvim).
func (a *App) Log(msg string) { a.log.Print("ui: ", msg) }

// Open asks nvim to edit the note with the given id.
func (a *App) Open(id string) error {
	a.mu.Lock()
	var n graph.Node
	var ok bool
	if a.graph != nil {
		n, ok = a.graph.Nodes[id]
	}
	a.mu.Unlock()
	if !ok {
		return errors.New("unknown note: " + id)
	}
	if _, err := os.Stat(n.Path); err != nil {
		return errors.New("no such file: " + n.Path) // ghost without a backing file
	}
	a.send(outMsg{Type: "open", Path: n.Path})
	return nil
}
