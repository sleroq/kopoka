package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

type artPanel struct {
	image       image.Image
	first, last int
}

// joinArtPanels only joins consecutive panels whose touching edges match at
// their original alignment. It never reorders, scales, crops, or adds gutters.
func joinArtPanels(images []image.Image) []artPanel {
	var panels []artPanel
	for first := 0; first < len(images); {
		panel := artPanel{image: images[first], first: first, last: first}
		for end := len(images); end > first+1; end-- {
			joined := joinArtGrid(images[first:end])
			if joined != nil {
				panel.image, panel.last = joined, end-1
				break
			}
		}
		panels = append(panels, panel)
		first = panel.last + 1
	}
	return panels
}

func joinArtGrid(images []image.Image) image.Image {
	columns := 0
	var size image.Point
	for candidate := 1; candidate <= len(images); candidate++ {
		if len(images)%candidate != 0 {
			continue
		}
		candidateSize, matches := artGridMatches(images, candidate)
		if !matches {
			continue
		}
		if columns != 0 { // Multiple plausible layouts are ambiguous.
			return nil
		}
		columns, size = candidate, candidateSize
	}
	if columns == 0 {
		return nil
	}

	joined := image.NewRGBA(image.Rectangle{Max: size})
	draw.Draw(joined, joined.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	offset := image.Point{}
	for i, img := range images {
		bounds := img.Bounds()
		rect := image.Rectangle{Min: offset, Max: offset.Add(bounds.Size())}
		draw.Draw(joined, rect, img, bounds.Min, draw.Over)
		offset.X += bounds.Dx()
		if (i+1)%columns == 0 {
			offset.X = 0
			offset.Y += bounds.Dy()
		}
	}
	return joined
}

func artGridMatches(images []image.Image, columns int) (image.Point, bool) {
	var size image.Point
	for i, img := range images {
		bounds := img.Bounds()
		row, column := i/columns, i%columns
		if bounds.Dx() != images[column].Bounds().Dx() || bounds.Dy() != images[row*columns].Bounds().Dy() {
			return image.Point{}, false
		}
		if row == 0 {
			size.X += bounds.Dx()
		}
		if column == 0 {
			size.Y += bounds.Dy()
		}
		// Check every original tile seam independently: a good match in one
		// column must never conceal an unrelated tile in another column.
		if column > 0 && !artSeamIsDistinct(images[i-1], img, true) {
			return image.Point{}, false
		}
		if row > 0 && !artSeamIsDistinct(images[i-columns], img, false) {
			return image.Point{}, false
		}
	}
	// Resizing can fit Telegram's dimensions, but cannot fix an aspect ratio over 20.
	return size, max(size.X, size.Y) <= 20*min(size.X, size.Y)
}

func artSeamIsDistinct(a, b image.Image, horizontal bool) bool {
	// Shared decorative borders often match in both directions without being
	// adjoining art. The intended direction must be unambiguous too.
	return artSeamMatches(a, b, horizontal) && !artSeamMatches(b, a, horizontal)
}

type edgeColor [3]float64

// A low color difference alone would join unrelated pictures with white borders
// or a shared palette. The aligned edge must also match much better than the
// same edge displaced along the seam, demonstrating spatial continuity.
func artSeamMatches(a, b image.Image, horizontal bool) bool {
	left := artEdgeProfile(a, horizontal, true)
	right := artEdgeProfile(b, horizontal, false)
	if len(left) < 16 || len(left) != len(right) {
		return false
	}
	aligned := edgeDifference(left, right, 0)
	if aligned > 10 {
		return false
	}
	displaced := math.Inf(1)
	for _, divisor := range []int{16, 8, 4} {
		shift := len(left) / divisor
		displaced = min(displaced, edgeDifference(left, right, shift), edgeDifference(left, right, len(left)-shift))
	}
	return displaced-aligned >= 8 && aligned <= 0.4*displaced
}

// Average four pixels along the touching edge to tolerate JPEG noise. Do not
// average inward: sharp detail away from the cut can make a true seam differ.
func artEdgeProfile(img image.Image, horizontal, trailing bool) []edgeColor {
	bounds := img.Bounds()
	length := bounds.Dx()
	if horizontal {
		length = bounds.Dy()
	}
	profile := make([]edgeColor, length/4)
	for i := range profile {
		for along := i * 4; along < (i+1)*4; along++ {
			x, y := bounds.Min.X+along, bounds.Min.Y
			if horizontal {
				x, y = bounds.Min.X, bounds.Min.Y+along
			}
			if trailing {
				if horizontal {
					x = bounds.Max.X - 1
				} else {
					y = bounds.Max.Y - 1
				}
			}
			r, g, b, _ := img.At(x, y).RGBA()
			profile[i][0] += float64(r) / 257
			profile[i][1] += float64(g) / 257
			profile[i][2] += float64(b) / 257
		}
		for c := range profile[i] {
			profile[i][c] /= 4
		}
	}
	return profile
}

func edgeDifference(a, b []edgeColor, shift int) float64 {
	var difference float64
	for i := range a {
		for c := range a[i] {
			difference += math.Abs(a[i][c] - b[(i+shift)%len(b)][c])
		}
	}
	return difference / float64(3*len(a))
}
