// zk-graph: a live graph view of a zk notebook, driven by Neovim.
//
// Started by the zk-graph.lua plugin (one process per notebook). Talks to nvim over
// stdin/stdout with newline-delimited JSON; everything else it logs goes to stderr.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

//go:embed all:frontend
var assets embed.FS

// stderrLogger keeps Wails' own logging off stdout, which carries the nvim protocol.
type stderrLogger struct{ l *log.Logger }

func (s stderrLogger) Print(m string)   { s.l.Print(m) }
func (s stderrLogger) Trace(m string)   { s.l.Print("TRC ", m) }
func (s stderrLogger) Debug(m string)   { s.l.Print("DEB ", m) }
func (s stderrLogger) Info(m string)    { s.l.Print("INF ", m) }
func (s stderrLogger) Warning(m string) { s.l.Print("WRN ", m) }
func (s stderrLogger) Error(m string)   { s.l.Print("ERR ", m) }
func (s stderrLogger) Fatal(m string)   { s.l.Fatal("FTL ", m) }

// parseHex turns "#rrggbb" into RGBA; anything else yields black.
func parseHex(s string) *options.RGBA {
	c := &options.RGBA{A: 255}
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return c
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return c
	}
	c.R, c.G, c.B = uint8(v>>16), uint8(v>>8), uint8(v)
	return c
}

func main() {
	var (
		root     = flag.String("root", "", "notebook root (required)")
		zkBin    = flag.String("zk", "zk", "zk executable")
		config   = flag.String("config", "{}", "frontend config as JSON (theme, filter, ...)")
		hidden   = flag.Bool("hidden", false, "start with the window hidden (shown on a \"show\" message)")
		appID    = flag.String("app-id", "zk-graph", "Wayland app_id / X11 WM_CLASS (for window rules)")
		title    = flag.String("title", "zk-graph", "window title")
		exts     = flag.String("ext", "md", "comma-separated note file extensions to watch")
		debounce = flag.Duration("debounce", 250*time.Millisecond, "quiet period before reloading after changes")
		gpu      = flag.String("gpu", "never", "webview GPU policy: always | ondemand | never")
		width    = flag.Int("width", 1200, "initial window width")
		height   = flag.Int("height", 850, "initial window height")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s --root DIR [flags] [-- extra zk graph args]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	lg := log.New(os.Stderr, "zk-graph: ", log.LstdFlags)

	if *root == "" {
		flag.Usage()
		os.Exit(2)
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		lg.Fatal(err)
	}
	if fi, err := os.Stat(absRoot); err != nil || !fi.IsDir() {
		lg.Fatalf("not a directory: %s", absRoot)
	}

	var cfg struct {
		Theme *struct {
			Bg string `json:"bg"`
		} `json:"theme"`
	}
	if err := json.Unmarshal([]byte(*config), &cfg); err != nil {
		lg.Fatalf("--config: %v", err)
	}
	bg := &options.RGBA{R: 255, G: 255, B: 255, A: 255}
	if cfg.Theme != nil {
		bg = parseHex(cfg.Theme.Bg) // avoids a white flash before the page paints
	}

	extSet := map[string]bool{}
	for _, e := range strings.Split(*exts, ",") {
		if e = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e)), "."); e != "" {
			extSet["."+e] = true
		}
	}

	policy := map[string]linux.WebviewGpuPolicy{
		"always": linux.WebviewGpuPolicyAlways, "ondemand": linux.WebviewGpuPolicyOnDemand,
		"never": linux.WebviewGpuPolicyNever,
	}[*gpu]

	sub, err := fs.Sub(assets, "frontend")
	if err != nil {
		lg.Fatal(err)
	}

	app := NewApp(Options{
		Root: absRoot, ZkBin: *zkBin, ZkArgs: flag.Args(), Config: json.RawMessage(*config),
		Exts: extSet, Debounce: *debounce,
	}, lg)

	err = wails.Run(&options.App{
		Title:              *title,
		Width:              *width,
		Height:             *height,
		MinWidth:           400,
		MinHeight:          300,
		StartHidden:        *hidden,
		HideWindowOnClose:  true, // closing hides; nvim owns the lifetime (stdin EOF quits)
		BackgroundColour:   bg,
		AssetServer:        &assetserver.Options{Assets: sub},
		OnStartup:          app.startup,
		Bind:               []interface{}{app},
		Logger:             stderrLogger{lg},
		LogLevel:           logger.WARNING,
		LogLevelProduction: logger.WARNING,
		Linux: &linux.Options{
			ProgramName:      *appID,
			WebviewGpuPolicy: policy,
		},
	})
	if err != nil {
		lg.Fatal(err)
	}
}
