// Package diagram lays out the compared images as an alignment graph, ready to render as
// SVG. Every pair of images that shares layers is connected by a line labeled with how well
// they align — the shared size as a fraction of the smaller image — and colored by it.
package diagram

import "math"

// Layer is one image layer (digest + compressed size).
type Layer struct {
	Digest string
	Size   int64
}

// Input is one image to place in the diagram.
type Input struct {
	Slot   string
	Name   string
	Total  int64 // total (compressed) size
	Layers []Layer
}

// Diagram is a laid-out graph ready for SVG rendering.
type Diagram struct {
	Width, Height int
	NodeW, NodeH  int
	Nodes         []Node
	Edges         []Edge
}

type Node struct {
	Slot string
	Name string
	X, Y int
}

type Edge struct {
	X1, Y1, X2, Y2 int
	MX, MY         int    // midpoint, for the alignment label
	Pct            int    // alignment: shared size / smaller image
	Class          string // low/mid/high, matching Pct
	Directed       bool   // true when one image is fully contained in the other (base → derived)
}

const (
	nodeW = 170
	nodeH = 62
	pad   = 16
)

type pt struct{ x, y int }

// Build places the images on a circle and connects every pair that shares layers. Returns
// nil for an empty input.
func Build(imgs []Input) *Diagram {
	n := len(imgs)
	if n == 0 {
		return nil
	}

	// Per-image digest -> size (deduplicated).
	sizes := make([]map[string]int64, n)
	for i := range imgs {
		m := make(map[string]int64, len(imgs[i].Layers))
		for _, l := range imgs[i].Layers {
			m[l.Digest] = l.Size
		}
		sizes[i] = m
	}

	// Circular layout: node centers evenly spaced, first at the top.
	r := 130 + n*16
	cx := r + nodeW/2 + pad
	cy := r + nodeH/2 + pad
	center := make([]pt, n)
	for i := range imgs {
		a := -math.Pi/2 + 2*math.Pi*float64(i)/float64(n)
		center[i] = pt{
			x: cx + int(math.Round(float64(r)*math.Cos(a))),
			y: cy + int(math.Round(float64(r)*math.Sin(a))),
		}
	}

	d := &Diagram{
		Width:  2*r + nodeW + 2*pad,
		Height: 2*r + nodeH + 2*pad,
		NodeW:  nodeW,
		NodeH:  nodeH,
	}
	for i := range imgs {
		d.Nodes = append(d.Nodes, Node{
			Slot: imgs[i].Slot,
			Name: imgs[i].Name,
			X:    center[i].x - nodeW/2,
			Y:    center[i].y - nodeH/2,
		})
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			shared := sharedSize(sizes[i], sizes[j])
			if shared <= 0 {
				continue // no shared layers → no connection
			}
			smaller := imgs[i].Total
			if imgs[j].Total < smaller {
				smaller = imgs[j].Total
			}
			// Ceil so any nonzero overlap shows as >= 1% (never a confusing 0% on a drawn
			// line). Full containment (shared == smaller) is the only true 100%; cap merely
			// near-full overlaps at 99% so they don't masquerade as a derivation.
			pct := 0
			full := false
			if smaller > 0 {
				full = shared == smaller
				pct = int((shared*100 + smaller - 1) / smaller)
				if !full && pct > 99 {
					pct = 99
				}
			}

			// Full containment with distinct sizes means the smaller image is fully inside
			// the larger — a derivation. Draw a directed arrow base → derived.
			base, derived := i, j
			directed := full && imgs[i].Total != imgs[j].Total
			if directed && imgs[j].Total < imgs[i].Total {
				base, derived = j, i
			}

			// Clip each endpoint to its node's border so lines connect box-edge to box-edge
			// (and arrowheads land on the derived box, not hidden under it).
			p1 := clip(center[base], center[derived])
			p2 := clip(center[derived], center[base])
			d.Edges = append(d.Edges, Edge{
				X1: p1.x, Y1: p1.y, X2: p2.x, Y2: p2.y,
				MX: (p1.x + p2.x) / 2, MY: (p1.y + p2.y) / 2,
				Pct:      pct,
				Class:    alignClass(pct),
				Directed: directed,
			})
		}
	}
	return d
}

// clip returns the point on the node rectangle centered at `from` where the segment toward
// `toward` crosses its border.
func clip(from, toward pt) pt {
	dx := float64(toward.x - from.x)
	dy := float64(toward.y - from.y)
	if dx == 0 && dy == 0 {
		return from
	}
	sx, sy := math.Inf(1), math.Inf(1)
	if dx != 0 {
		sx = (float64(nodeW) / 2) / math.Abs(dx)
	}
	if dy != 0 {
		sy = (float64(nodeH) / 2) / math.Abs(dy)
	}
	s := math.Min(sx, sy)
	return pt{x: from.x + int(math.Round(dx*s)), y: from.y + int(math.Round(dy*s))}
}

// sharedSize sums the sizes of layer digests present in both images.
func sharedSize(a, b map[string]int64) int64 {
	if len(b) < len(a) {
		a, b = b, a
	}
	var total int64
	for digest, size := range a {
		if _, ok := b[digest]; ok {
			total += size
		}
	}
	return total
}

// alignClass buckets an alignment percentage into the low/mid/high palette.
func alignClass(pct int) string {
	switch {
	case pct >= 40:
		return "high"
	case pct >= 20:
		return "mid"
	default:
		return "low"
	}
}
