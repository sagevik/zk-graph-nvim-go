-- zk-graph.lua - Neovim side of zk-graph (Go/Wails live graph view for zk notebooks).
--
-- One zk-graph process per notebook, spawned by this nvim and talking over stdin/stdout
-- (newline-delimited JSON). Because the process is our child, "open note" always lands in
-- this nvim, and closing stdin (nvim exit or crash) makes it quit.
--
--   nvim -> app  {"type":"show"|"hide"|"refresh"|"quit"} {"type":"current","id":"rel/path.md"}
--   app -> nvim  {"type":"ready","notes":N} {"type":"open","path":"/abs"} {"type":"error",...}
--
-- Requires: nvim >= 0.10, zk-nvim, `zk` and the `zk-graph` binary on $PATH.
-- Setup: require("zk-graph").setup({ ... })  (see M.config)

local uv = vim.uv
local M = {}

M.config = {
  bin = "zk-graph", -- the Go/Wails binary
  zk_cmd = "zk",
  zk_args = {}, -- extra `zk graph` flags, e.g. { "--exclude", "journal" }
  lsp_name = "zk", -- zk-nvim's LSP client name; its attach marks "inside a notebook"
  autostart = true, -- start hidden when the zk LSP attaches, so :ZkGraph shows instantly
  title = "zk-graph", -- window title
  app_id = "zk-graph", -- Wayland app_id / X11 WM_CLASS, for compositor window rules
  ext = { "md" }, -- note extensions to watch
  debounce_ms = 250, -- quiet period after file changes before re-reading the graph
  gpu = "never", -- webview GPU policy: "always" | "ondemand" | "never"
  width = 1200,
  height = 850,
  env = {}, -- extra environment, e.g. { WEBKIT_DISABLE_DMABUF_RENDERER = "1" }

  -- view
  follow = true, -- centre the graph on the note in the current buffer
  tag_colors = true,
  legend_open = true, -- filter panel expanded at start
  directed = false, -- draw link arrows
  stabilize_iterations = 200, -- layout steps before first paint; it keeps settling after
  color_tags = {}, -- tags that decide node color first, in priority order
  color_max_share = 0.5, -- skip tags on more than this share of notes when picking a color
  filter = { top = 12 }, -- most used tags shown as buttons; the rest via search
  theme = {
    enabled = true, -- false = plain light look
    bg = nil, -- nil = your Normal background (black if transparent)
    palette = {}, -- override vague.nvim colors by name, e.g. { hint = "#89b4fa" }
    accents = nil, -- node colors in rank order; nil = derived from the palette
    edge = nil, -- nil = palette.floatBorder
    edge_opacity = 0.8,
    edge_width = 1,
  },

  ---@type fun(path: string)|nil  called after a note is opened (e.g. focus the terminal)
  on_open = nil,
}

---@class ZkGraphProc
---@field root string
---@field real_root string
---@field obj vim.SystemObj?
---@field ready boolean
---@field partial string
---@field log string[]
---@field current string?
---@field stopping boolean
---@field last_error string?

---@type table<string, ZkGraphProc>
local procs = {}

local function notify(msg, level)
  vim.notify("zk-graph: " .. msg, level or vim.log.levels.INFO)
end

-- theme -------------------------------------------------------------------------------

-- vague.nvim defaults (lua/vague/config/internal.lua). If vague is loaded, its live colors
-- (including your overrides) take precedence; M.config.theme.palette overrides both.
local VAGUE = {
  bg = "#141415", inactiveBg = "#1c1c24", fg = "#cdcdcd", floatBorder = "#878787",
  line = "#252530", comment = "#606079", builtin = "#b4d4cf", func = "#c48282",
  string = "#e8b589", number = "#e0a363", property = "#c3c3d5", constant = "#aeaed1",
  parameter = "#bb9dbd", visual = "#333738", error = "#d8647e", warning = "#f3be7c",
  hint = "#7e98e8", operator = "#90a0b5", keyword = "#6e94b2", type = "#9bb4bc",
  search = "#405065", plus = "#7fa563", delta = "#f3be7c",
}
-- ordered for contrast between neighbours: the biggest groups get the most distinct hues
local ACCENT_KEYS = {
  "hint", "string", "plus", "error", "parameter", "builtin",
  "func", "keyword", "number", "constant", "type", "warning",
}

local function hl_bg(name)
  local ok, hl = pcall(vim.api.nvim_get_hl, 0, { name = name, link = false })
  return ok and hl.bg and ("#%06x"):format(hl.bg) or nil
end

local function resolve_theme()
  local t = M.config.theme
  if not t or t.enabled == false then return nil end
  local p = vim.deepcopy(VAGUE)
  local okv, vc = pcall(require, "vague.config.internal")
  if okv and type(vc) == "table" and type(vc.current) == "table" and type(vc.current.colors) == "table" then
    p = vim.tbl_extend("force", p, vc.current.colors)
  end
  p = vim.tbl_extend("force", p, t.palette or {})
  return {
    bg = t.bg or hl_bg("Normal") or "#000000",
    panel = p.inactiveBg,
    field = p.bg,
    fg = p.fg,
    muted = p.comment,
    border = p.line,
    edge = t.edge or p.floatBorder,
    edge_opacity = t.edge_opacity or 0.8,
    edge_width = t.edge_width or 1,
    focus = p.hint,
    danger = p.error,
    accents = t.accents or vim.tbl_map(function(k) return p[k] end, ACCENT_KEYS),
  }
end

local function frontend_config()
  local c = M.config
  return {
    theme = resolve_theme(),
    filter = { top = c.filter.top },
    legend_open = c.legend_open,
    tag_colors = c.tag_colors,
    follow = c.follow,
    directed = c.directed,
    stabilize_iterations = c.stabilize_iterations,
    color_tags = c.color_tags,
    color_max_share = c.color_max_share,
  }
end

-- notebook / path helpers ---------------------------------------------------------------

local function notebook_root(bufnr)
  local ok, util = pcall(require, "zk.util")
  if not ok then return nil end
  local path = vim.api.nvim_buf_get_name(bufnr or 0)
  local root = util.notebook_root(path ~= "" and path or vim.fn.getcwd())
    or util.notebook_root(util.resolve_notebook_path(bufnr or 0) or vim.fn.getcwd())
  return root and vim.fs.normalize(root) or nil
end

---@return ZkGraphProc?, string?  the process owning `path` and the notebook-relative id
local function proc_for_path(path)
  local real = path ~= "" and uv.fs_realpath(path) or nil
  if not real then return nil end
  for _, p in pairs(procs) do
    if real:sub(1, #p.real_root + 1) == p.real_root .. "/" then
      return p, real:sub(#p.real_root + 2)
    end
  end
end

local function is_note(path)
  local ext = path:match("%.([^./]+)$")
  return ext ~= nil and vim.tbl_contains(M.config.ext, ext:lower())
end

-- messaging -----------------------------------------------------------------------------

local function send(p, msg)
  if not p.obj or p.stopping then return end
  local ok, err = pcall(p.obj.write, p.obj, vim.json.encode(msg) .. "\n")
  if not ok then p.log[#p.log + 1] = "write failed: " .. tostring(err) end
end

local function send_current(p, bufnr)
  local path = vim.api.nvim_buf_get_name(bufnr or 0)
  if path == "" or not is_note(path) then return end
  local owner, id = proc_for_path(path)
  if owner ~= p or not id or id == p.current then return end
  p.current = id
  send(p, { type = "current", id = id })
end

-- note opening --------------------------------------------------------------------------

local function is_normal_win(win)
  local buf = vim.api.nvim_win_get_buf(win)
  return vim.api.nvim_win_get_config(win).relative == "" and vim.bo[buf].buftype == ""
end

---Prefer the current window; fall back to the first normal window in the tab so a click
---doesn't replace a quickfix/terminal/picker float.
local function target_win()
  local cur = vim.api.nvim_get_current_win()
  if is_normal_win(cur) then return cur end
  for _, w in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if is_normal_win(w) then return w end
  end
  return cur
end

local function open_note(p, path)
  local real = uv.fs_realpath(path)
  if not real then return notify("no such file: " .. path, vim.log.levels.WARN) end
  if real:sub(1, #p.real_root + 1) ~= p.real_root .. "/" then
    return notify("refusing path outside notebook: " .. real, vim.log.levels.WARN)
  end
  vim.api.nvim_set_current_win(target_win())
  local ok, err = pcall(vim.cmd.edit, vim.fn.fnameescape(real))
  if not ok then return notify(tostring(err), vim.log.levels.WARN) end
  if M.config.on_open then pcall(M.config.on_open, real) end
end

local function handle(p, line)
  local ok, msg = pcall(vim.json.decode, line)
  if not ok or type(msg) ~= "table" then
    p.log[#p.log + 1] = "stdout: " .. line -- stray output; never act on it
    return
  end
  if msg.type == "ready" then
    p.ready = true
    p.current = nil
    send_current(p, 0)
  elseif msg.type == "open" and type(msg.path) == "string" then
    open_note(p, msg.path)
  elseif msg.type == "error" and type(msg.message) == "string" then
    if msg.message ~= p.last_error then -- don't repeat the same failure on every reload
      notify(msg.message, vim.log.levels.WARN)
    end
    p.last_error = msg.message
  end
end

-- process lifecycle ---------------------------------------------------------------------

local LOG_MAX = 200

---Starts (or reuses) the process for `root`.
---@return ZkGraphProc?, boolean? started
local function ensure(root, visible)
  if procs[root] then return procs[root], false end
  local c = M.config
  if vim.fn.executable(c.bin) == 0 then
    notify(c.bin .. " not found in $PATH (build it, or set `bin`)", vim.log.levels.ERROR)
    return nil
  end

  local cmd = {
    c.bin, "--root", root, "--zk", c.zk_cmd,
    "--config", vim.json.encode(frontend_config()),
    "--title", c.title, "--app-id", c.app_id,
    "--ext", table.concat(c.ext, ","), "--gpu", c.gpu,
    "--debounce", ("%dms"):format(c.debounce_ms),
    "--width", tostring(c.width), "--height", tostring(c.height),
  }
  if not visible then cmd[#cmd + 1] = "--hidden" end
  if #c.zk_args > 0 then
    cmd[#cmd + 1] = "--"
    vim.list_extend(cmd, c.zk_args)
  end

  ---@type ZkGraphProc
  local p = {
    root = root, real_root = uv.fs_realpath(root) or root,
    ready = false, partial = "", log = {}, stopping = false,
  }

  local ok, obj = pcall(vim.system, cmd, {
    stdin = true,
    text = true,
    env = next(c.env) and c.env or nil,
    -- both callbacks run in a fast (luv) context: buffer here, act on the main loop
    stdout = function(_, data)
      if not data then return end
      p.partial = p.partial .. data
      while true do
        local nl = p.partial:find("\n", 1, true)
        if not nl then break end
        local line = p.partial:sub(1, nl - 1)
        p.partial = p.partial:sub(nl + 1)
        if line ~= "" then vim.schedule(function() handle(p, line) end) end
      end
    end,
    stderr = function(_, data)
      if not data then return end
      for line in data:gmatch("[^\n]+") do
        p.log[#p.log + 1] = line
      end
      while #p.log > LOG_MAX do table.remove(p.log, 1) end
    end,
  }, function(res)
    vim.schedule(function()
      if procs[root] == p then procs[root] = nil end
      if not p.stopping and res.code ~= 0 then
        local tail = table.concat(vim.list_slice(p.log, math.max(1, #p.log - 4)), "\n")
        notify(("exited with code %d (see :ZkGraphLog)\n%s"):format(res.code, tail), vim.log.levels.ERROR)
      end
    end)
  end)
  if not ok then
    notify("failed to start: " .. tostring(obj), vim.log.levels.ERROR)
    return nil
  end
  p.obj = obj
  procs[root] = p
  return p, true
end

local function stop(p)
  if not p or p.stopping then return end
  send(p, { type = "quit" })
  p.stopping = true
  pcall(p.obj.write, p.obj, nil) -- close stdin: EOF also makes the app quit
end

local function current_proc(create_visible)
  local root = notebook_root(0)
  if not root then
    notify("no zk notebook for this buffer or cwd", vim.log.levels.ERROR)
    return nil
  end
  if create_visible ~= nil then return ensure(root, create_visible) end
  return procs[root]
end

-- public API ----------------------------------------------------------------------------

---Show the graph for the current notebook, starting it if needed.
function M.open()
  local p, started = current_proc(true)
  if p and not started then
    send(p, { type = "show" })
    send_current(p, 0)
  end
end

function M.hide()
  local p = current_proc()
  if p then send(p, { type = "hide" }) end
end

function M.refresh()
  local p = current_proc()
  if p then send(p, { type = "refresh" }) end
end

function M.stop()
  local p = current_proc()
  if p then stop(p) end
end

function M.stop_all()
  for _, p in pairs(procs) do stop(p) end
end

---Show the process log (stderr) for the current notebook in a scratch buffer.
function M.log()
  local p = current_proc()
  if not p then return notify("not running for this notebook") end
  vim.cmd("botright new")
  local buf = vim.api.nvim_get_current_buf()
  vim.bo[buf].buftype, vim.bo[buf].bufhidden, vim.bo[buf].swapfile = "nofile", "wipe", false
  vim.api.nvim_buf_set_name(buf, "zk-graph://log")
  vim.api.nvim_buf_set_lines(buf, 0, -1, false, #p.log > 0 and p.log or { "(empty)" })
  vim.bo[buf].modifiable = false
end

function M.setup(opts)
  M.config = vim.tbl_deep_extend("force", M.config, opts or {})

  local cmd = vim.api.nvim_create_user_command
  cmd("ZkGraph", M.open, { desc = "Show the zk graph for this notebook" })
  cmd("ZkGraphHide", M.hide, { desc = "Hide the zk graph window" })
  cmd("ZkGraphRefresh", M.refresh, { desc = "Re-read the zk graph now" })
  cmd("ZkGraphStop", M.stop, { desc = "Quit the zk graph process for this notebook" })
  cmd("ZkGraphLog", M.log, { desc = "Show the zk graph process log" })

  local group = vim.api.nvim_create_augroup("zk-graph", { clear = true })

  -- "inside a notebook" = zk-nvim attached its LSP client to the buffer
  vim.api.nvim_create_autocmd("LspAttach", {
    group = group,
    callback = function(args)
      if not M.config.autostart then return end
      local client = vim.lsp.get_client_by_id(args.data.client_id)
      if not client or client.name ~= M.config.lsp_name then return end
      local root = notebook_root(args.buf)
      if root then ensure(root, false) end
    end,
  })

  vim.api.nvim_create_autocmd("BufEnter", {
    group = group,
    callback = function(args)
      local path = vim.api.nvim_buf_get_name(args.buf)
      if path == "" or not is_note(path) then return end
      local p = proc_for_path(path)
      if p and p.ready then send_current(p, args.buf) end
    end,
  })

  vim.api.nvim_create_autocmd("VimLeavePre", { group = group, callback = M.stop_all })
end

return M
