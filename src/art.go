package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"slices"
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
	drawArtGrid(joined, images, columns, draw.Over)
	return joined
}

func drawArtGrid(dst draw.Image, images []image.Image, columns int, op draw.Op) {
	offset := image.Point{}
	for i, img := range images {
		bounds := img.Bounds()
		rect := image.Rectangle{Min: offset, Max: offset.Add(bounds.Size())}
		draw.Draw(dst, rect, img, bounds.Min, op)
		offset.X += bounds.Dx()
		if (i+1)%columns == 0 {
			offset.X = 0
			offset.Y += bounds.Dy()
		}
	}
}

func artGridSize(images []image.Image, columns int) (image.Point, bool) {
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
	}
	return size, true
}

func artGridMatches(images []image.Image, columns int) (image.Point, bool) {
	size, fits := artGridSize(images, columns)
	if !fits {
		return image.Point{}, false
	}
	for i, img := range images {
		row, column := i/columns, i%columns
		// Check every original tile seam independently: a good match in one
		// column must never conceal an unrelated tile in another column.
		if column > 0 && !artSeamIsDistinct(images[i-1], img, true) {
			if columns != len(images) || !artRowMatches(images) {
				return image.Point{}, false
			}
			break
		}
		if row > 0 && !artSeamIsDistinct(images[i-columns], img, false) {
			return image.Point{}, false
		}
	}
	// Resizing can fit Telegram's dimensions, but cannot fix an aspect ratio over 20.
	return size, max(size.X, size.Y) <= 20*min(size.X, size.Y)
}

// Repeated interior edges need directional seams at both ends of the row.
func artRowMatches(images []image.Image) bool {
	for i := 1; i < len(images); i++ {
		forward := artSeamScore(images[i-1], images[i], true, true)
		if math.IsInf(forward, 1) {
			return false
		}
		if i == 1 || i == len(images)-1 {
			reverse := artSeamScore(images[i], images[i-1], true, true)
			if forward >= reverse || forward > 0.5*reverse {
				return false
			}
		}
	}
	return true
}

func artSeamIsDistinct(a, b image.Image, horizontal bool) bool {
	forward := artSeamScore(a, b, horizontal, false)
	if math.IsInf(forward, 1) {
		return false
	}
	reverse := artSeamScore(b, a, horizontal, false)
	return forward < reverse && forward <= 0.5*reverse
}

type edgeColor [3]float64

func artSeamMatches(a, b image.Image, horizontal bool) bool {
	return !math.IsInf(artSeamScore(a, b, horizontal, false), 1)
}

// The aligned edge must also beat displacement along the seam, demonstrating
// spatial continuity rather than just a shared palette or border.
func artSeamScore(a, b image.Image, horizontal, relaxed bool) float64 {
	left := artEdgeProfile(a, horizontal, true)
	right := artEdgeProfile(b, horizontal, false)
	if len(left) < 16 || len(left) != len(right) {
		return math.Inf(1)
	}
	mean := edgeDifference(left, right, 0, false)
	for _, trimmed := range []bool{false, true} {
		aligned := mean
		if trimmed {
			aligned = edgeDifference(left, right, 0, true)
		}
		if aligned > 10 {
			continue
		}
		displaced := math.Inf(1)
		for _, divisor := range []int{16, 8, 4} {
			shift := len(left) / divisor
			displaced = min(displaced,
				edgeDifference(left, right, shift, trimmed),
				edgeDifference(left, right, len(left)-shift, trimmed),
			)
		}
		if displaced-aligned >= 8 && aligned <= 0.4*displaced ||
			!trimmed && aligned <= 3 && displaced-aligned >= 0.4 && aligned <= 0.8*displaced ||
			relaxed && displaced-aligned >= 3 && (aligned <= 0.4*displaced || !trimmed && aligned <= (2.0/3)*displaced) {
			return mean
		}
	}
	return math.Inf(1)
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

func edgeDifference(a, b []edgeColor, shift int, trimmed bool) float64 {
	differences := make([]float64, len(a))
	for i := range a {
		for c := range a[i] {
			differences[i] += math.Abs(a[i][c]-b[(i+shift)%len(b)][c]) / 3
		}
	}
	if trimmed {
		// Sharp contours can change abruptly across a genuine cut. The best
		// half must still beat displaced edges under the same trimmed score.
		slices.Sort(differences)
		differences = differences[:len(differences)/2]
	}
	var difference float64
	for _, value := range differences {
		difference += value
	}
	return difference / float64(len(differences))
}
