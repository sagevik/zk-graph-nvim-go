# zk-graph

A live graph view for [zk](https://github.com/zk-org/zk) notebooks, driven from Neovim.
Go + Wails (WebKitGTK) window, vis-network rendering, no browser.

- Starts **hidden** when zk-nvim's LSP attaches (i.e. only inside a notebook), so
  `:ZkGraph` shows instantly.
- Watches the notebook and applies **incremental updates**: added/changed/removed notes and
  links are patched into the running graph, layout positions are kept.
- Click a node: the note opens in the Neovim that owns the window.
- Follows the current buffer, optional local scope (1 to 3 links around it), tag filter
  over all tags (top tags + search), theme inspired from the vague.nvim colorscheme.

## Build

Requirements: Go >= 1.22, GTK3 and WebKitGTK 4.1 development files.

```sh
# Arch
sudo pacman -S --needed go gtk3 webkit2gtk-4.1
# Debian/Ubuntu
sudo apt install golang libgtk-3-dev libwebkit2gtk-4.1-dev

make deps      # go mod tidy (fetches Wails v2 + fsnotify, writes go.sum)
make test
make install   # -> ~/.local/bin/zk-graph
make install-plugin   # -> ~/.config/nvim/lua/zk-graph.lua (replaces the Chromium-based one)
```

`make debug` builds with the WebKit inspector enabled (right-click, Inspect Element).

## Neovim

Needs zk-nvim with its LSP enabled (the attach is the "inside a notebook" signal).

```lua
require("zk-graph").setup({
  theme = { bg = "#000000" },
  -- env = { WEBKIT_DISABLE_DMABUF_RENDERER = "1" }, -- if the window stays blank
})
vim.keymap.set("n", "<leader>zg", "<cmd>ZkGraph<cr>")
```

| Command           | Effect                                                   |
|-------------------|----------------------------------------------------------|
| `:ZkGraph`        | show the graph for the current notebook (start if needed) |
| `:ZkGraphHide`    | hide the window (closing it does the same)               |
| `:ZkGraphRefresh` | re-read the graph now                                    |
| `:ZkGraphStop`    | quit the process for this notebook                       |
| `:ZkGraphLog`     | show the process log (stderr) in a scratch buffer        |

All options and defaults are at the top of `nvim/lua/zk-graph.lua` (`M.config`). Notable:
`autostart`, `follow`, `color_tags` (priority list deciding node color),
`color_max_share` (ignore catch-all tags when coloring), `zk_args` (e.g. `--exclude`),
`gpu` (`never` by default, see below), `on_open` (e.g. focus your terminal after a click).

In the window: `/` search tags, `f` fit, `c` centre on the current note.

## Window rules (mango, Hyprland, ...)

The app_id / WM_CLASS is `zk-graph` (option `app_id`), stable across launches:

```
windowrule=isfloating:1,width:1100,height:850,appid:^zk-graph$
```

## Protocol

One process per notebook, child of the nvim that started it. Newline-delimited JSON:

```
nvim -> app   {"type":"show"} {"type":"hide"} {"type":"refresh"} {"type":"quit"}
              {"type":"current","id":"notebook/relative/path.md"}
app -> nvim   {"type":"ready","notes":N} {"type":"open","path":"/abs/path.md"}
              {"type":"error","message":"..."}
```

stdin EOF (nvim exits or crashes) quits the app. Everything else goes to stderr.

The binary also runs standalone: `zk-graph --root ~/notes` (`--help` for flags).

## Troubleshooting

- **Blank or black window**: WebKitGTK's DMA-BUF renderer misbehaves on some GPU/driver
  combinations (notably NVIDIA on Wayland). Set
  `env = { WEBKIT_DISABLE_DMABUF_RENDERER = "1" }`.
- **Sluggish with large notebooks**: try `gpu = "always"` (hardware-accelerated
  compositing; off by default because it is the setting most likely to break). Lower
  `stabilize_iterations` for a faster first paint.
- **No live updates**: check `:ZkGraphLog` for watcher errors; very large trees can hit
  `fs.inotify.max_user_watches`.
- **Window does not come to the front on `:ZkGraph`**: Wayland does not let clients raise
  themselves; use a compositor rule or keybinding.
