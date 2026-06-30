package diagram

import "testing"

// findEdgePct returns the Pct of the edge connecting the nodes at centers matching the
// given midpoint search is overkill; instead we just collect all Pcts.
func pcts(d *Diagram) []int {
	out := make([]int, 0, len(d.Edges))
	for _, e := range d.Edges {
		out = append(out, e.Pct)
	}
	return out
}

func TestBuildAlignment(t *testing.T) {
	// X, Y, Z all share a "base" layer. Y and Z add their own.
	x := Input{Slot: "X", Name: "x", Total: 50, Layers: []Layer{{"base", 50}}}
	y := Input{Slot: "Y", Name: "y", Total: 100, Layers: []Layer{{"base", 50}, {"y", 50}}}
	z := Input{Slot: "Z", Name: "z", Total: 200, Layers: []Layer{{"base", 50}, {"z", 150}}}

	d := Build([]Input{x, y, z})
	if d == nil || len(d.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %v", d)
	}
	// Every pair shares "base" (50), so all three pairs are connected.
	if len(d.Edges) != 3 {
		t.Fatalf("expected 3 edges, got %d", len(d.Edges))
	}
	// X↔Y and X↔Z: base is 100% of the smaller image X (50/50).
	// Y↔Z: base is 50% of the smaller image Y (50/100).
	got := pcts(d)
	count100, count50 := 0, 0
	directed := 0
	for _, e := range d.Edges {
		switch e.Pct {
		case 100:
			count100++
		case 50:
			count50++
		}
		if e.Directed {
			directed++
		}
	}
	if count100 != 2 || count50 != 1 {
		t.Errorf("alignment pcts = %v, want two 100%% and one 50%%", got)
	}
	// The two 100% links are derivations (X fully inside Y and Z) → directed arrows.
	// The 50% link (Y↔Z) is a plain line.
	if directed != 2 {
		t.Errorf("directed edges = %d, want 2", directed)
	}
}

func TestTinyOverlapShowsOnePercent(t *testing.T) {
	// A shares just a 1-byte base with B, which is a vanishing fraction — must show 1%, not 0%.
	a := Input{Slot: "A", Name: "a", Total: 1000000, Layers: []Layer{{"base", 1}, {"a", 999999}}}
	b := Input{Slot: "B", Name: "b", Total: 2000000, Layers: []Layer{{"base", 1}, {"b", 1999999}}}
	d := Build([]Input{a, b, {Slot: "C", Name: "c", Total: 5, Layers: []Layer{{"base", 1}, {"c", 4}}}})
	for _, e := range d.Edges {
		if e.Pct < 1 {
			t.Errorf("edge Pct = %d, want >= 1 for a nonzero overlap", e.Pct)
		}
	}
}

func TestNearFullNotDerivation(t *testing.T) {
	// B contains 99.9% of A by size but not all of it → 99%, plain line (no arrow).
	a := Input{Slot: "A", Name: "a", Total: 1000, Layers: []Layer{{"base", 999}, {"x", 1}}}
	b := Input{Slot: "B", Name: "b", Total: 2000, Layers: []Layer{{"base", 999}, {"y", 1001}}}
	d := Build([]Input{a, b, {Slot: "C", Name: "c", Total: 3000, Layers: []Layer{{"base", 999}, {"z", 2001}}}})
	for _, e := range d.Edges {
		if e.Pct == 100 || e.Directed {
			t.Errorf("near-full overlap should not be 100%%/directed, got pct=%d directed=%v", e.Pct, e.Directed)
		}
	}
}

func TestBuildNoShare(t *testing.T) {
	// No shared digests → no edges.
	imgs := []Input{
		{Slot: "A", Name: "a", Total: 10, Layers: []Layer{{"1", 10}}},
		{Slot: "B", Name: "b", Total: 20, Layers: []Layer{{"2", 20}}},
		{Slot: "C", Name: "c", Total: 30, Layers: []Layer{{"3", 30}}},
	}
	if d := Build(imgs); len(d.Edges) != 0 {
		t.Errorf("expected 0 edges for unrelated images, got %d", len(d.Edges))
	}
}

func TestAlignClass(t *testing.T) {
	cases := map[int]string{5: "low", 25: "mid", 55: "high", 100: "high"}
	for pct, want := range cases {
		if got := alignClass(pct); got != want {
			t.Errorf("alignClass(%d) = %q, want %q", pct, got, want)
		}
	}
}
