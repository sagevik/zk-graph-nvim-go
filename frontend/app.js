"use strict";
// zk-graph frontend. State flow:
//   Go --graph:reset--> Init() snapshot     (first load, or after a missed diff)
//   Go --graph:diff---> applyDiff()         (incremental; layout positions are kept)
//   nvim --note:current--> setCurrent()     (highlight / follow / local scope)
//   click --> App.Open(id) --> nvim :edit
(function () {
  const $ = (id) => document.getElementById(id);
  const api = () => window.go.main.App;
  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  const LIGHT = {
    bg: "#ffffff", panel: "#ffffff", field: "#f6f8fa", fg: "#24292f", muted: "#8c959f",
    border: "#d0d7de", edge: "#8c959f", edge_opacity: 0.6, edge_width: 1, focus: "#0969da",
    accents: ["#0969da", "#bf8700", "#1a7f37", "#cf222e", "#8250df", "#1b7c83",
      "#bc4c00", "#4d2d8f", "#953800", "#116329", "#a40e26", "#0550ae"],
  };

  // ---- state ----------------------------------------------------------------------------
  let cfg = null; // from nvim (via Go), fixed per process
  let T = LIGHT; // theme
  let version = 0;
  let model = { nodes: new Map(), edges: new Map() };
  let counts = new Map(); // tag -> number of notes
  let order = []; // tags by count desc, then name
  let adj = new Map(); // id -> Set of neighbour ids (undirected)
  let noteCount = 0; // notes, excluding ghosts
  const sel = new Set(); // selected tags, in selection order
  let mode = "any";
  let scope = 0; // 0 = all notes, n = within n links of the current note
  let colorsOn = true;
  let follow = true;
  let current = "";
  let network = null, visNodes = null, visEdges = null;
  let resyncing = false;
  let userMoved = false; // user panned/zoomed: don't auto-fit over their view
  let settled = false; // physics at rest
  let pendingCentre = false; // re-centre once the layout settles
  let queued = []; // diffs that arrived during a resync

  // ---- ui helpers ------------------------------------------------------------------------
  function banner(msg) {
    const b = $("banner");
    b.textContent = msg || "";
    b.hidden = !msg;
  }
  let toastTimer = 0;
  function toast(msg) {
    const t = $("toast");
    t.textContent = msg;
    t.classList.add("on");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove("on"), 2500);
  }
  function plural(n, word) { return n + " " + word + (n === 1 ? "" : "s"); }

  function applyTheme() {
    T = Object.assign({}, LIGHT, cfg.theme || {});
    if (!Array.isArray(T.accents) || !T.accents.length) T.accents = LIGHT.accents;
    const root = document.documentElement;
    for (const k of ["bg", "panel", "field", "fg", "muted", "border", "focus"]) {
      root.style.setProperty("--" + k, T[k]);
    }
    if (T.danger) root.style.setProperty("--danger", T.danger);
    root.style.colorScheme = cfg.theme ? "dark" : "light"; // native select/datalist popups
  }

  // ---- model -----------------------------------------------------------------------------
  function reindex() {
    counts = new Map();
    adj = new Map();
    noteCount = 0;
    for (const n of model.nodes.values()) {
      adj.set(n.id, new Set());
      if (!n.ghost) noteCount++;
      for (const t of n.tags) counts.set(t, (counts.get(t) || 0) + 1);
    }
    for (const e of model.edges.values()) {
      if (adj.has(e.from)) adj.get(e.from).add(e.to);
      if (adj.has(e.to)) adj.get(e.to).add(e.from);
    }
    order = [...counts.keys()].sort((a, b) => counts.get(b) - counts.get(a) || a.localeCompare(b));
    for (const t of [...sel]) if (!counts.has(t)) sel.delete(t); // tag no longer exists
    const dl = $("zkf-all");
    dl.textContent = "";
    for (const t of order) {
      const o = document.createElement("option");
      o.value = t;
      o.label = plural(counts.get(t), "note");
      dl.appendChild(o);
    }
    $("zkf-search").placeholder = "Search " + plural(order.length, "tag");
  }

  // The tag that decides a note's color: the first selected tag it has while filtering,
  // else the first match in cfg.color_tags, else its most widely used tag.
  function colorKey(n) {
    if (n.ghost || !n.tags.length) return null;
    if (sel.size) {
      for (const t of sel) if (n.tags.includes(t)) return t;
      return null;
    }
    for (const t of cfg.color_tags || []) if (n.tags.includes(t)) return t;
    // Tags on more than color_max_share of all notes (e.g. a catch-all #notes) say little
    // about a note; prefer the most used tag below that share, else the most used overall.
    const cap = (cfg.color_max_share ?? 0.5) * noteCount;
    let best = null, fallback = null;
    const better = (a, b) => b === null || counts.get(a) > counts.get(b) ||
      (counts.get(a) === counts.get(b) && a < b);
    for (const t of n.tags) {
      if (better(t, fallback)) fallback = t;
      if (counts.get(t) <= cap && better(t, best)) best = t;
    }
    return best ?? fallback;
  }

  // Accents go to the color keys in use, most important first, so the most frequent
  // groups get the most distinct hues. Past the palette size, colors repeat.
  function colorMap() {
    const keys = new Set();
    for (const n of model.nodes.values()) {
      const k = colorKey(n);
      if (k) keys.add(k);
    }
    const pri = cfg.color_tags || [];
    const ranked = sel.size
      ? [...sel].filter((t) => keys.has(t))
      : [...pri.filter((t) => keys.has(t)), ...order.filter((t) => keys.has(t) && !pri.includes(t))];
    const m = new Map();
    ranked.forEach((t, i) => m.set(t, T.accents[i % T.accents.length]));
    return m;
  }

  function neighbourhood(id, depth) {
    const seen = new Set([id]);
    let frontier = [id];
    for (let d = 0; d < depth; d++) {
      const next = [];
      for (const x of frontier) {
        for (const nb of adj.get(x) || []) {
          if (!seen.has(nb)) { seen.add(nb); next.push(nb); }
        }
      }
      frontier = next;
    }
    return seen;
  }

  function visibleSet() {
    const vis = new Set();
    const tags = [...sel];
    const local = scope > 0 && current && model.nodes.has(current) ? neighbourhood(current, scope) : null;
    for (const n of model.nodes.values()) {
      if (n.ghost || (local && !local.has(n.id))) continue;
      if (tags.length && !(local && n.id === current)) { // the local centre always stays
        const ok = mode === "all" ? tags.every((t) => n.tags.includes(t))
          : tags.some((t) => n.tags.includes(t));
        if (!ok) continue;
      }
      vis.add(n.id);
    }
    for (const n of model.nodes.values()) { // ghosts: shown when linked to a shown note
      if (!n.ghost || (local && !local.has(n.id))) continue;
      for (const nb of adj.get(n.id) || []) if (vis.has(nb)) { vis.add(n.id); break; }
    }
    return vis;
  }

  // ---- vis -------------------------------------------------------------------------------
  function tooltip(n) {
    const el = document.createElement("div");
    const add = (cls, text) => {
      const d = document.createElement("div");
      d.className = cls;
      d.textContent = text;
      el.appendChild(d);
    };
    add("tip-title", n.title);
    add("tip-meta", n.id);
    if (n.ghost) {
      add("tip-meta", "Outside the current zk selection");
    } else {
      if (n.tags.length) add("tip-meta", n.tags.map((t) => "#" + t).join(" "));
      add("tip-meta", plural(n.backlinks, "backlink") + ", " + plural(n.words, "word"));
    }
    return el;
  }

  function visNode(n) {
    return { id: n.id, label: n.title, value: n.backlinks, title: tooltip(n),
      shape: "dot", size: n.ghost ? 5 : undefined };
  }
  function visEdge(e) { return { id: e.id, from: e.from, to: e.to }; }

  function nodeStyle(n, vis, cmap) {
    const isCur = n.id === current;
    let bg, border;
    if (n.ghost) {
      bg = T.bg;
      border = T.muted;
    } else {
      bg = (colorsOn && cmap.get(colorKey(n))) || T.muted;
      border = isCur ? T.fg : bg;
    }
    return {
      id: n.id,
      hidden: !vis.has(n.id),
      borderWidth: isCur ? 3 : 1,
      borderWidthSelected: 3,
      color: { background: bg, border, highlight: { background: bg, border: T.fg },
        hover: { background: bg, border: T.fg } },
    };
  }

  function options() {
    return {
      autoResize: true,
      layout: { improvedLayout: false }, // its seeding pass is slow on large graphs
      nodes: {
        shape: "dot",
        borderWidth: 1,
        scaling: { min: 7, max: 28, label: { enabled: true, min: 12, max: 16, drawThreshold: 7 } },
        font: { color: T.fg, face: "system-ui, sans-serif", strokeWidth: 3, strokeColor: T.bg },
      },
      edges: {
        color: { color: T.edge, highlight: T.fg, hover: T.fg, inherit: false, opacity: T.edge_opacity },
        width: T.edge_width,
        selectionWidth: (w) => w + 1,
        smooth: false,
        arrows: { to: { enabled: !!cfg.directed, scaleFactor: 0.4 } },
      },
      physics: {
        solver: "forceAtlas2Based",
        forceAtlas2Based: { gravitationalConstant: -45, centralGravity: 0.01, springLength: 90,
          springConstant: 0.06, avoidOverlap: 0.3 },
        maxVelocity: 60,
        minVelocity: 1.5, // rest threshold: lower = finer final layout, longer settling
        stabilization: { iterations: cfg.stabilize_iterations ?? 200, updateInterval: 25, fit: true },
      },
      interaction: { hover: true, tooltipDelay: 350, multiselect: false },
    };
  }

  function createNetwork() {
    visNodes = new vis.DataSet();
    visEdges = new vis.DataSet();
    network = new vis.Network($("graph"), { nodes: visNodes, edges: visEdges }, options());
    network.on("click", (p) => {
      if (p.nodes && p.nodes.length) {
        api().Open(String(p.nodes[0])).catch((e) => toast(String(e)));
      }
    });
    network.on("stabilizationProgress", (p) => {
      $("loading").hidden = false;
      $("loading").textContent = "Laying out " + plural(model.nodes.size, "note") + " (" +
        Math.round((100 * p.iterations) / p.total) + "%)";
    });
    network.on("dragEnd", (p) => { if (!p.nodes || !p.nodes.length) userMoved = true; });
    network.on("zoom", () => { userMoved = true; });
    const done = () => { $("loading").hidden = true; };
    network.on("stabilizationIterationsDone", done);
    network.on("startStabilizing", () => { settled = false; });
    network.on("stabilized", () => {
      settled = true;
      done();
      if (pendingCentre) { pendingCentre = false; centre(!reduceMotion); }
    });
  }

  function refresh() {
    if (!network) return;
    const vis = visibleSet();
    const cmap = colorMap();
    visNodes.update([...model.nodes.values()].map((n) => nodeStyle(n, vis, cmap)));
    renderPanel(cmap, vis);
  }

  // ---- sync with Go ----------------------------------------------------------------------
  function loadSnapshot(s) {
    const pos = network.getPositions(); // keep the layout across a resync
    model = {
      nodes: new Map(s.nodes.map((n) => [n.id, n])),
      edges: new Map(s.edges.map((e) => [e.id, e])),
    };
    version = s.version;
    reindex();
    visEdges.clear();
    visNodes.clear();
    visNodes.add(s.nodes.map((n) => Object.assign(visNode(n), pos[n.id] || {})));
    visEdges.add(s.edges.map(visEdge));
    refresh();
    $("loading").hidden = true; // stabilizationProgress re-shows it for long layouts
    // vis only pre-stabilizes on setData; nodes added to a live DataSet would start from a
    // pile at the origin. Run the pass explicitly (it fits the view when done).
    if (s.nodes.length) network.stabilize(cfg.stabilize_iterations ?? 200);
    // Physics keeps spreading the graph after the stabilization pass (which fits to the
    // still-compact layout), so frame it again once it comes to rest.
    if (!userMoved) pendingCentre = true;
    if (!s.nodes.length) {
      $("loading").hidden = false;
      $("loading").textContent = "No notes found";
    }
  }

  async function resync() {
    if (resyncing) return;
    resyncing = true;
    try {
      const p = await api().Init();
      if (!cfg) {
        cfg = p.config || {};
        applyTheme();
        createNetwork();
        colorsOn = cfg.tag_colors !== false;
        follow = cfg.follow !== false;
        setOpen(cfg.legend_open !== false);
      }
      banner(p.error);
      current = p.current || "";
      if (p.graph) {
        loadSnapshot(p.graph);
      } else {
        $("loading").hidden = false; // first zk run still going; graph:reset follows
      }
    } catch (e) {
      banner("zk-graph: " + e);
    } finally {
      resyncing = false;
    }
    const q = queued;
    queued = [];
    for (const d of q) if (d.version > version) applyDiff(d);
  }

  // Place a new node next to an already positioned neighbour instead of at the origin.
  function seedPosition(n, pos) {
    for (const nb of adj.get(n.id) || []) {
      const p = pos[nb];
      if (p) return { x: p.x + (Math.random() - 0.5) * 60, y: p.y + (Math.random() - 0.5) * 60 };
    }
    return {};
  }

  function applyDiff(d) {
    if (resyncing) { queued.push(d); return; }
    if (!network || d.base !== version) { resync(); return; } // missed an update
    const added = d.upsertNodes.filter((n) => !model.nodes.has(n.id));
    for (const n of d.upsertNodes) model.nodes.set(n.id, n);
    for (const id of d.removeNodes) model.nodes.delete(id);
    for (const e of d.addEdges) model.edges.set(e.id, e);
    for (const id of d.removeEdges) model.edges.delete(id);
    version = d.version;
    reindex();

    const pos = network.getPositions();
    visEdges.remove(d.removeEdges);
    visNodes.remove(d.removeNodes);
    visNodes.update(d.upsertNodes.map((n) =>
      Object.assign(visNode(n), model.nodes.has(n.id) && !pos[n.id] ? seedPosition(n, pos) : {})));
    visEdges.add(d.addEdges.map(visEdge));
    refresh();

    const parts = [];
    const notes = (list) => list.filter((x) => !(x.ghost)).length;
    if (added.length) parts.push(notes(added) + " added");
    const changed = d.upsertNodes.length - added.length;
    if (changed) parts.push(changed + " changed");
    if (d.removeNodes.length) parts.push(d.removeNodes.length + " removed");
    if (d.addEdges.length || d.removeEdges.length) {
      parts.push("links +" + d.addEdges.length + " -" + d.removeEdges.length);
    }
    if (parts.length) toast("Updated: " + parts.join(", "));
  }

  function focusCurrent(animate) {
    if (!network || !current || !visNodes.get(current) || visNodes.get(current).hidden) return false;
    network.selectNodes([current]);
    network.focus(current, { scale: Math.max(network.getScale(), 0.6),
      animation: animate && !reduceMotion ? { duration: 450, easingFunction: "easeInOutQuad" } : false });
    return true;
  }

  function setCurrent(id) {
    current = id || "";
    refresh();
    if (!follow) return;
    focusCurrent(true);
    if (!settled) pendingCentre = true;
  }

  // Re-centre (on the current note when following, else fit) - once the layout has
  // settled if it is still moving: WebKit pauses timers in hidden windows, so a graph that
  // loaded while hidden only finishes its layout after being shown, and a node centred
  // mid-layout drifts away from the centre.
  function centre(animate) {
    if (!network) return;
    if (!(follow && focusCurrent(animate)) && !userMoved) network.fit();
  }
  function centreWhenSettled() {
    centre(false);
    if (!settled) pendingCentre = true;
  }

  // Shown after starting hidden (or after a hide): wait until the webview has its size.
  function onShown() {
    setTimeout(() => {
      if (!network) return;
      network.redraw();
      centreWhenSettled();
    }, 60);
  }

  // ---- panel -----------------------------------------------------------------------------
  function chip(tag, removable, cmap) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "zkf-chip";
    b.setAttribute("aria-pressed", sel.has(tag) ? "true" : "false");
    b.title = plural(counts.get(tag) || 0, "note");
    const c = colorsOn && cmap.get(tag);
    if (c) {
      const dot = document.createElement("span");
      dot.className = "zkf-dot";
      dot.style.background = c;
      b.appendChild(dot);
    }
    b.appendChild(document.createTextNode(tag));
    const n = document.createElement("span");
    n.className = "n";
    n.textContent = removable ? "\u00d7" : String(counts.get(tag) || 0);
    b.appendChild(n);
    b.onclick = () => toggle(tag);
    return b;
  }

  function renderPanel(cmap, vis) {
    const topN = order.slice(0, (cfg.filter && cfg.filter.top) || 12);
    const t = $("zkf-top"), x = $("zkf-extra");
    t.textContent = "";
    x.textContent = "";
    for (const tag of topN) t.appendChild(chip(tag, false, cmap));
    for (const tag of [...sel].filter((s) => !topN.includes(s))) x.appendChild(chip(tag, true, cmap));
    x.hidden = !x.childNodes.length;

    const active = sel.size + (scope > 0 ? 1 : 0);
    $("zkf-toggle").textContent = active ? "Filter (" + active + ")" : "Filter";
    $("zkf-toggle").dataset.active = active ? "true" : "false";
    $("zkf-mode").value = mode;
    $("zkf-scope").value = String(scope);
    $("zkf-colors").setAttribute("aria-pressed", String(colorsOn));
    $("zkf-follow").setAttribute("aria-pressed", String(follow));

    let total = 0, shown = 0;
    for (const n of model.nodes.values()) {
      if (n.ghost) continue;
      total++;
      if (vis.has(n.id)) shown++;
    }
    $("zkf-count").textContent = shown === total ? plural(total, "note") : shown + " of " + plural(total, "note");
  }

  function toggle(tag) {
    if (sel.has(tag)) sel.delete(tag); else sel.add(tag);
    refresh();
  }

  function pick(tag) {
    if (!tag || !counts.has(tag)) return;
    sel.add(tag);
    $("zkf-search").value = "";
    refresh();
  }

  const panel = $("zkf-panel"), toggleBtn = $("zkf-toggle"), search = $("zkf-search");
  function setOpen(o) {
    panel.hidden = !o;
    toggleBtn.setAttribute("aria-expanded", String(o));
  }
  toggleBtn.onclick = () => { setOpen(panel.hidden); if (!panel.hidden) search.focus(); };

  search.addEventListener("input", (e) => {
    if (e.inputType === "insertReplacementText" || e.inputType === undefined) pick(search.value.trim());
  });
  search.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { search.value = ""; search.blur(); return; }
    if (e.key !== "Enter") return;
    const q = search.value.trim().toLowerCase();
    if (!q) return;
    pick(order.find((t) => t.toLowerCase() === q) ||
      order.find((t) => t.toLowerCase().startsWith(q)) ||
      order.find((t) => t.toLowerCase().includes(q)));
  });

  $("zkf-mode").onchange = (e) => { mode = e.target.value; refresh(); };
  $("zkf-scope").onchange = (e) => {
    scope = Number(e.target.value);
    if (scope > 0 && !current) toast("Open a note in nvim to centre the local view");
    refresh();
    if (network) network.fit({ animation: reduceMotion ? false : { duration: 400 } });
  };
  $("zkf-clear").onclick = () => { sel.clear(); scope = 0; refresh(); };
  $("zkf-colors").onclick = () => { colorsOn = !colorsOn; refresh(); };
  $("zkf-follow").onclick = () => { follow = !follow; refresh(); if (follow) setCurrent(current); };
  $("zkf-fit").onclick = () => network && network.fit({ animation: reduceMotion ? false : { duration: 400 } });

  // keys: "/" search, "f" fit, "c" centre on the current note
  document.addEventListener("keydown", (e) => {
    if (/^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement.tagName) || e.ctrlKey || e.altKey || e.metaKey) return;
    if (e.key === "/") { e.preventDefault(); setOpen(true); search.focus(); }
    else if (e.key === "f" && network) network.fit();
    else if (e.key === "c" && network && current && visNodes.get(current)) {
      network.focus(current, { scale: Math.max(network.getScale(), 0.8), animation: !reduceMotion });
    }
  });

  // ---- boot ------------------------------------------------------------------------------
  function boot() {
    if (!window.runtime || !window.go || !window.go.main) { setTimeout(boot, 20); return; }
    // subscribe first, then pull: anything emitted in between is caught by the version check
    window.runtime.EventsOn("graph:reset", () => resync());
    window.runtime.EventsOn("graph:diff", (d) => applyDiff(d));
    window.runtime.EventsOn("graph:error", (msg) => banner(msg));
    window.runtime.EventsOn("note:current", (id) => setCurrent(id));
    window.runtime.EventsOn("window:show", onShown);
    resync();
  }
  boot();
})();
