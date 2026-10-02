package graph

import (
	"testing"
)

const sample = `{
  "notes": [
    {"path": "a.md", "absPath": "/nb/a.md", "title": "A", "filenameStem": "a", "tags": ["x", "y"], "wordCount": 3},
    {"path": "sub/b c.md", "absPath": "/nb/sub/b c.md", "title": "", "filenameStem": "b c", "tags": null},
    {"path": "c.md", "absPath": "/nb/c.md", "title": "C", "tags": ["x"]}
  ],
  "links": [
    {"sourcePath": "a.md", "targetPath": "sub/b c.md"},
    {"sourcePath": "a.md", "targetPath": "sub/b c.md"},
    {"sourcePath": "c.md", "targetPath": "sub/b c.md"},
    {"sourcePath": "c.md", "targetPath": "c.md"},
    {"sourcePath": "c.md", "targetPath": ""},
    {"sourcePath": "a.md", "targetPath": "outside.md"}
  ]
}`

func mustParse(t *testing.T, s string) *Graph {
	t.Helper()
	g, err := Parse([]byte(s), "/nb")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestParse(t *testing.T) {
	g := mustParse(t, sample)

	if len(g.Nodes) != 4 {
		t.Fatalf("nodes = %d, want 4 (3 notes + 1 ghost)", len(g.Nodes))
	}
	b := g.Nodes["sub/b c.md"]
	if b.Title != "b c" {
		t.Errorf("empty title should fall back to filename stem, got %q", b.Title)
	}
	if b.Tags == nil {
		t.Error("nil tags must become an empty slice (JSON [] not null)")
	}
	if b.Backlinks != 2 {
		t.Errorf("backlinks = %d, want 2 (duplicate link collapsed)", b.Backlinks)
	}
	ghost := g.Nodes["outside.md"]
	if !ghost.Ghost || ghost.Path != "/nb/outside.md" || ghost.Title != "outside" {
		t.Errorf("ghost node wrong: %+v", ghost)
	}
	if len(g.Edges) != 3 {
		t.Errorf("edges = %d, want 3 (dup, self and unresolved links dropped)", len(g.Edges))
	}
	if _, ok := g.Edges[EdgeID("c.md", "c.md")]; ok {
		t.Error("self link should be dropped")
	}
}

func TestComputeInitialIsFullAdd(t *testing.T) {
	g := mustParse(t, sample)
	d := Compute(nil, g, 0)
	if d.Base != 0 || d.Version != 1 {
		t.Errorf("versions = %d->%d, want 0->1", d.Base, d.Version)
	}
	if len(d.UpsertNodes) != len(g.Nodes) || len(d.AddEdges) != len(g.Edges) {
		t.Errorf("initial diff should add everything: %+v", d)
	}
}

func TestComputeNoChange(t *testing.T) {
	if d := Compute(mustParse(t, sample), mustParse(t, sample), 5); !d.Empty() {
		t.Errorf("identical graphs should give an empty diff: %+v", d)
	}
}

func TestComputeChanges(t *testing.T) {
	old := mustParse(t, sample)
	next := mustParse(t, `{
  "notes": [
    {"path": "a.md", "absPath": "/nb/a.md", "title": "A renamed", "tags": ["x", "y"], "wordCount": 3},
    {"path": "c.md", "absPath": "/nb/c.md", "title": "C", "tags": ["x", "z"]},
    {"path": "d.md", "absPath": "/nb/d.md", "title": "D", "tags": []}
  ],
  "links": [
    {"sourcePath": "d.md", "targetPath": "a.md"}
  ]
}`)
	d := Compute(old, next, 3)

	up := map[string]bool{}
	for _, n := range d.UpsertNodes {
		up[n.ID] = true
	}
	// a: title + backlinks changed, c: tags changed, d: new
	for _, id := range []string{"a.md", "c.md", "d.md"} {
		if !up[id] {
			t.Errorf("%s should be upserted", id)
		}
	}
	if len(d.RemoveNodes) != 2 { // "sub/b c.md" and ghost "outside.md"
		t.Errorf("removeNodes = %v", d.RemoveNodes)
	}
	if len(d.AddEdges) != 1 || d.AddEdges[0].ID != EdgeID("d.md", "a.md") {
		t.Errorf("addEdges = %v", d.AddEdges)
	}
	if len(d.RemoveEdges) != 3 {
		t.Errorf("removeEdges = %v", d.RemoveEdges)
	}
	if d.Base != 3 || d.Version != 4 {
		t.Errorf("versions = %d->%d", d.Base, d.Version)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("not json"), "/nb"); err == nil {
		t.Error("expected error")
	}
}
