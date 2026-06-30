// Package compare computes how much layer data a set of images share, and provides the
// size/time formatting helpers used to present the result.
package compare

import (
	"fmt"
	"time"

	"dic/internal/registry"
)

// ImageSummary describes one image's layer set for a given platform.
type ImageSummary struct {
	Ref       string
	Total     int64 // sum of all (unique) layer sizes
	NumLayers int
	Shared    int64 // size of this image's layers also present in another compared image
	Unique    int64 // size of this image's layers present in no other compared image
}

// Comparison holds the result of comparing two or more images on a shared platform.
type Comparison struct {
	Images    []ImageSummary
	Shared    int64 // total size of layers present in *all* images (matched by digest)
	NumShared int
}

// SharedPct returns the shared size as a percentage of the smallest image's total.
// Using the smallest total makes "100% shared" mean the smallest image is fully
// contained in every other one.
func (c Comparison) SharedPct() float64 {
	if len(c.Images) == 0 {
		return 0
	}
	min := c.Images[0].Total
	for _, im := range c.Images[1:] {
		if im.Total < min {
			min = im.Total
		}
	}
	if min == 0 {
		return 0
	}
	return float64(c.Shared) / float64(min) * 100
}

// SharedClass buckets SharedPct into a severity class for color-coding:
// "low" (<20%), "mid" (<40%), or "high".
func (c Comparison) SharedClass() string {
	switch p := c.SharedPct(); {
	case p < 20:
		return "low"
	case p < 40:
		return "mid"
	default:
		return "high"
	}
}

// layerSizes folds a layer slice into a digest->size map, deduplicating repeated digests
// (a layer reused within one image counts once toward its total).
func layerSizes(layers []registry.Layer) map[string]int64 {
	m := make(map[string]int64, len(layers))
	for _, l := range layers {
		m[l.Digest] = l.Size
	}
	return m
}

func sumSizes(m map[string]int64) int64 {
	var total int64
	for _, s := range m {
		total += s
	}
	return total
}

// Images computes the shared size across two or more images' layer sets — the total size
// of layers (by digest) present in every image — plus each image's shared/unique split.
func Images(refs []string, layerSets [][]registry.Layer) Comparison {
	n := len(layerSets)
	maps := make([]map[string]int64, n)
	summaries := make([]ImageSummary, n)
	for i, ls := range layerSets {
		m := layerSizes(ls)
		maps[i] = m
		summaries[i] = ImageSummary{Ref: refs[i], Total: sumSizes(m), NumLayers: len(m)}
	}

	// Count how many images contain each digest. A digest in >= 2 images is "shared".
	counts := make(map[string]int)
	for _, m := range maps {
		for d := range m {
			counts[d]++
		}
	}

	// Per-image shared/unique split (docker system df semantics).
	for i := range summaries {
		var sh, uq int64
		for d, size := range maps[i] {
			if counts[d] >= 2 {
				sh += size
			} else {
				uq += size
			}
		}
		summaries[i].Shared = sh
		summaries[i].Unique = uq
	}

	// Top-line shared: size of layers present in *every* image (intersection).
	var shared int64
	var numShared int
	if n > 0 {
		for digest, size := range maps[0] {
			if counts[digest] == n {
				shared += size
				numShared++
			}
		}
	}

	return Comparison{Images: summaries, Shared: shared, NumShared: numShared}
}

// HumanSize formats a byte count as a human-readable string (e.g. "180.4 MB").
func HumanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// RelativeAge renders a coarse "… ago" phrase for a past timestamp.
func RelativeAge(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		return "in the future"
	}
	const day = 24 * time.Hour
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return countUnit(int(d/time.Minute), "minute")
	case d < day:
		return countUnit(int(d/time.Hour), "hour")
	case d < 30*day:
		return countUnit(int(d/day), "day")
	case d < 365*day:
		return countUnit(int(d/(30*day)), "month")
	default:
		return countUnit(int(d/(365*day)), "year")
	}
}

func countUnit(n int, unit string) string {
	if n < 1 {
		n = 1
	}
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}
