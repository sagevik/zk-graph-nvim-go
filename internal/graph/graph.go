// Package graph turns `zk graph --format=json` output into a node/edge model and
// computes minimal diffs between two models, so the UI can update in place.
package graph

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Node is a note (or a ghost: a link endpoint outside the current zk result set).
type Node struct {
	ID        string   `json:"id"`   // notebook-relative path, as reported by zk
	Title     string   `json:"title"`
	Path      string   `json:"path"` // absolute path
	Tags      []string `json:"tags"`
	Backlinks int      `json:"backlinks"`
	Words     int      `json:"words"`
	Modified  string   `json:"modified,omitempty"`
	Ghost     bool     `json:"ghost,omitempty"`
}

// Edge is a directed link between two notes; parallel links are collapsed.
type Edge struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph is an immutable snapshot of the notebook's link structure.
type Graph struct {
	Nodes map[string]Node
	Edges map[string]Edge
}

// Snapshot is the full graph as sent to the UI.
type Snapshot struct {
	Version int    `json:"version"`
	Nodes   []Node `json:"nodes"`
	Edges   []Edge `json:"edges"`
}

// Diff transforms the graph at version Base into the graph at Version.
type Diff struct {
	Base        int      `json:"base"`
	Version     int      `json:"version"`
	UpsertNodes []Node   `json:"upsertNodes"`
	RemoveNodes []string `json:"removeNodes"`
	AddEdges    []Edge   `json:"addEdges"`
	RemoveEdges []string `json:"removeEdges"`
}

// Empty reports whether the diff changes nothing.
func (d Diff) Empty() bool {
	return len(d.UpsertNodes) == 0 && len(d.RemoveNodes) == 0 &&
		len(d.AddEdges) == 0 && len(d.RemoveEdges) == 0
}

type zkNote struct {
	Path         string   `json:"path"`
	AbsPath      string   `json:"absPath"`
	Title        string   `json:"title"`
	FilenameStem string   `json:"filenameStem"`
	Tags         []string `json:"tags"`
	WordCount    int      `json:"wordCount"`
	Modified     string   `json:"modified"`
}

type zkLink struct {
	SourcePath string `json:"sourcePath"`
	TargetPath string `json:"targetPath"`
}

type zkGraph struct {
	Notes []zkNote `json:"notes"`
	Links []zkLink `json:"links"`
}

// edgeSep cannot appear in file paths, so edge ids are unambiguous.
const edgeSep = "\x1f"

// EdgeID returns the id of the edge from -> to.
func EdgeID(from, to string) string { return from + edgeSep + to }

// Parse builds a Graph from `zk graph --format=json` output. root is the notebook root,
// used for ghost nodes whose absolute path zk does not report.
func Parse(data []byte, root string) (*Graph, error) {
	var raw zkGraph
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse zk graph json: %w", err)
	}
	g := &Graph{Nodes: make(map[string]Node, len(raw.Notes)), Edges: make(map[string]Edge, len(raw.Links))}

	for _, n := range raw.Notes {
		if n.Path == "" {
			continue
		}
		title := strings.TrimSpace(n.Title)
		if title == "" {
			title = n.FilenameStem
		}
		abs := n.AbsPath
		if abs == "" {
			abs = filepath.Join(root, n.Path)
		}
		tags := n.Tags
		if tags == nil {
			tags = []string{}
		}
		g.Nodes[n.Path] = Node{ID: n.Path, Title: title, Path: abs, Tags: tags, Words: n.WordCount, Modified: n.Modified}
	}

	for _, l := range raw.Links {
		if l.SourcePath == "" || l.TargetPath == "" || l.SourcePath == l.TargetPath {
			continue // unresolved or self link
		}
		for _, p := range []string{l.SourcePath, l.TargetPath} {
			if _, ok := g.Nodes[p]; !ok { // endpoint outside a filtered result set
				stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
				g.Nodes[p] = Node{ID: p, Title: stem, Path: filepath.Join(root, p), Tags: []string{}, Ghost: true}
			}
		}
		id := EdgeID(l.SourcePath, l.TargetPath)
		if _, dup := g.Edges[id]; dup {
			continue
		}
		g.Edges[id] = Edge{ID: id, From: l.SourcePath, To: l.TargetPath}
		n := g.Nodes[l.TargetPath]
		n.Backlinks++
		g.Nodes[l.TargetPath] = n
	}
	return g, nil
}

// Snapshot returns the graph as sorted slices (deterministic output).
func (g *Graph) Snapshot(version int) Snapshot {
	s := Snapshot{Version: version, Nodes: make([]Node, 0, len(g.Nodes)), Edges: make([]Edge, 0, len(g.Edges))}
	for _, n := range g.Nodes {
		s.Nodes = append(s.Nodes, n)
	}
	for _, e := range g.Edges {
		s.Edges = append(s.Edges, e)
	}
	sort.Slice(s.Nodes, func(i, j int) bool { return s.Nodes[i].ID < s.Nodes[j].ID })
	sort.Slice(s.Edges, func(i, j int) bool { return s.Edges[i].ID < s.Edges[j].ID })
	return s
}

func nodeEqual(a, b Node) bool {
	return a.Title == b.Title && a.Path == b.Path && a.Backlinks == b.Backlinks &&
		a.Words == b.Words && a.Modified == b.Modified && a.Ghost == b.Ghost &&
		slices.Equal(a.Tags, b.Tags)
}

// Compute returns the diff from old (at version base) to new (at base+1).
// A nil old graph diffs against an empty graph.
func Compute(old, new *Graph, base int) Diff {
	if old == nil {
		old = &Graph{Nodes: map[string]Node{}, Edges: map[string]Edge{}}
	}
	d := Diff{Base: base, Version: base + 1, UpsertNodes: []Node{}, RemoveNodes: []string{},
		AddEdges: []Edge{}, RemoveEdges: []string{}}
	for id, n := range new.Nodes {
		if o, ok := old.Nodes[id]; !ok || !nodeEqual(o, n) {
			d.UpsertNodes = append(d.UpsertNodes, n)
		}
	}
	for id := range old.Nodes {
		if _, ok := new.Nodes[id]; !ok {
			d.RemoveNodes = append(d.RemoveNodes, id)
		}
	}
	for id, e := range new.Edges {
		if _, ok := old.Edges[id]; !ok {
			d.AddEdges = append(d.AddEdges, e)
		}
	}
	for id := range old.Edges {
		if _, ok := new.Edges[id]; !ok {
			d.RemoveEdges = append(d.RemoveEdges, id)
		}
	}
	sort.Slice(d.UpsertNodes, func(i, j int) bool { return d.UpsertNodes[i].ID < d.UpsertNodes[j].ID })
	sort.Strings(d.RemoveNodes)
	sort.Slice(d.AddEdges, func(i, j int) bool { return d.AddEdges[i].ID < d.AddEdges[j].ID })
	sort.Strings(d.RemoveEdges)
	return d
}
