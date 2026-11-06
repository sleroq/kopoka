package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"slices"
	"testing"
)

func artScene(bounds image.Rectangle, phase float64) *image.RGBA {
	img := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			fx, fy := float64(x), float64(y)
			pixel := color.RGBA{
				R: uint8(120 + 55*math.Sin(fx*0.031+fy*0.057+phase)),
				G: uint8(120 + 65*math.Sin(fx*0.051-fy*0.033+phase)),
				B: uint8(120 + 60*math.Sin(fx*0.071+fy*0.041+phase)),
				A: 255,
			}
			img.SetRGBA(x, y, pixel)
		}
	}
	return img
}

func TestJoinArtPanels(t *testing.T) {
	scene := artScene(image.Rect(13, 17, 813, 657), 0)
	bounds := scene.Bounds()

	tests := []struct {
		name   string
		panels []image.Rectangle
	}{
		{"horizontal unequal widths", []image.Rectangle{
			image.Rect(13, 17, 273, 657), image.Rect(273, 17, 493, 657), image.Rect(493, 17, 813, 657),
		}},
		{"vertical unequal heights", []image.Rectangle{
			image.Rect(13, 17, 813, 297), image.Rect(13, 297, 813, 657),
		}},
		{"row major grid", []image.Rectangle{
			image.Rect(13, 17, 413, 337), image.Rect(413, 17, 813, 337),
			image.Rect(13, 337, 413, 657), image.Rect(413, 337, 813, 657),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var images []image.Image
			for _, rect := range tt.panels {
				images = append(images, scene.SubImage(rect))
			}
			got := joinArtPanels(images)
			if len(got) != 1 {
				t.Fatalf("got %d groups, want one", len(got))
			}
			if got[0].first != 0 || got[0].last != len(images)-1 {
				t.Fatalf("unexpected source range: %d..%d", got[0].first, got[0].last)
			}
			joined := got[0].image
			if joined.Bounds() != (image.Rectangle{Max: bounds.Size()}) {
				t.Fatalf("bounds = %v, want size %v", joined.Bounds(), bounds.Size())
			}
			for y := range bounds.Dy() {
				for x := range bounds.Dx() {
					if joined.At(x, y) != scene.At(x+bounds.Min.X, y+bounds.Min.Y) {
						t.Fatalf("pixel at %d,%d changed or a gap was inserted", x, y)
					}
				}
			}
		})
	}
}

func TestJoinArtPanelsRejectsUnrelated(t *testing.T) {
	bounds := image.Rect(0, 0, 200, 320)
	first := artScene(bounds, 0)
	second := artScene(bounds, 2)
	white := image.NewRGBA(bounds)
	draw.Draw(white, bounds, image.White, image.Point{}, draw.Src)
	bordered := artScene(bounds, 0)
	draw.Draw(bordered, image.Rect(0, 0, 3, 320), image.White, image.Point{}, draw.Src)
	sharedPatch := artScene(bounds, 2)
	draw.Draw(sharedPatch, image.Rect(0, 0, 1, 80), first, image.Pt(199, 0), draw.Src)
	tests := []struct {
		name   string
		images []image.Image
	}{
		{"same dimensions and palette", []image.Image{first, second}},
		{"blank matching borders", []image.Image{white, bordered}},
		{"blank panels", []image.Image{white, white}},
		{"small shared edge patch", []image.Image{first, sharedPatch}},
		{"single picture", []image.Image{first}},
		{"empty post", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := joinArtPanels(tt.images)
			if len(got) != len(tt.images) {
				t.Fatalf("got %d groups, want %d", len(got), len(tt.images))
			}
			for i := range got {
				if got[i].image != tt.images[i] || got[i].first != i || got[i].last != i {
					t.Fatalf("original picture %d was changed or reordered", i)
				}
			}
		})
	}
}

func TestJoinArtPanelsPreservesSeparateArt(t *testing.T) {
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	unrelated := artScene(image.Rect(0, 0, 200, 320), 2)
	got := joinArtPanels([]image.Image{
		scene.SubImage(image.Rect(0, 0, 200, 320)),
		scene.SubImage(image.Rect(200, 0, 400, 320)),
		unrelated,
	})
	if len(got) != 2 || got[0].first != 0 || got[0].last != 1 || got[1].image != unrelated {
		t.Fatalf("split art and unrelated picture were not kept distinct: %+v", got)
	}
}

func TestJoinArtPanelsJPEG(t *testing.T) {
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	var images []image.Image
	for _, rect := range []image.Rectangle{image.Rect(0, 0, 200, 320), image.Rect(200, 0, 400, 320)} {
		var encoded bytes.Buffer
		err := jpeg.Encode(&encoded, scene.SubImage(rect), &jpeg.Options{Quality: 85})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := jpeg.Decode(&encoded)
		if err != nil {
			t.Fatal(err)
		}
		images = append(images, decoded)
	}
	if got := joinArtPanels(images); len(got) != 1 {
		t.Fatalf("JPEG panels produced %d groups, want one", len(got))
	}
}

func TestArtEdgeProfileIgnoresInterior(t *testing.T) {
	bounds := image.Rect(13, 17, 213, 337)
	original := artScene(bounds, 0)
	changed := image.NewRGBA(bounds)
	draw.Draw(changed, bounds, original, bounds.Min, draw.Src)
	draw.Draw(changed, bounds.Inset(1), image.Black, image.Point{}, draw.Src)

	for _, horizontal := range []bool{false, true} {
		for _, trailing := range []bool{false, true} {
			before := artEdgeProfile(original, horizontal, trailing)
			after := artEdgeProfile(changed, horizontal, trailing)
			if !slices.Equal(before, after) {
				t.Fatalf("interior detail changed the edge profile: horizontal=%v trailing=%v", horizontal, trailing)
			}
		}
	}
}

func TestJoinArtPanelsReportedPosts(t *testing.T) {
	// Fixtures retain only the outer three columns of each original panel,
	// including their exact JPEG-decoded pixels, rather than the full artwork.
	tests := []struct {
		post, fixture string
		widths        []int
		height        int
	}{
		{"onlymikufans/status/2106005292253364454", "triptych-edges.png", []int{832, 607, 609}, 1024},
		{"onlymikufans/status/2113237963190792414", "four-panel-edges.png", []int{262, 511, 708, 439}, 1080},
		{"310ri_310/status/2116470643172757896", "310ri-triptych-edges.png", []int{946, 946, 946}, 2048},
	}

	for _, tt := range tests {
		t.Run(tt.post, func(t *testing.T) {
			// Source: https://x.com/ + tt.post.
			file, err := os.Open("testdata/" + tt.fixture)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			edges, err := png.Decode(file)
			if err != nil {
				t.Fatal(err)
			}

			var images []image.Image
			width := 0
			for i, panelWidth := range tt.widths {
				panel := image.NewRGBA(image.Rect(0, 0, panelWidth, tt.height))
				draw.Draw(panel, image.Rect(0, 0, 3, tt.height), edges, image.Pt(i*6, 0), draw.Src)
				draw.Draw(panel, image.Rect(panelWidth-3, 0, panelWidth, tt.height), edges, image.Pt(i*6+3, 0), draw.Src)
				images = append(images, panel)
				width += panelWidth
			}
			got := joinArtPanels(images)
			if len(got) != 1 || got[0].image.Bounds() != image.Rect(0, 0, width, tt.height) {
				t.Fatalf("reported panels were not joined into %dx%d: %+v", width, tt.height, got)
			}
			for i, a := range images {
				for j, b := range images {
					if j <= i+1 {
						continue
					}
					if artSeamIsDistinct(a, b, true) {
						t.Errorf("unrelated edges of panels %d and %d matched", i+1, j+1)
					}
				}
			}
		})
	}
}

func TestJoinArtGridRejectsPartiallyMatchingBoundary(t *testing.T) {
	scene := artScene(image.Rect(0, 0, 800, 640), 0)
	bottomRight := image.NewRGBA(image.Rect(0, 0, 400, 320))
	draw.Draw(bottomRight, bottomRight.Bounds(), scene, image.Pt(400, 320), draw.Src)
	for y := range 3 {
		for x := range 400 {
			pixel := bottomRight.RGBAAt(x, y)
			pixel.R += 11
			pixel.G += 11
			pixel.B += 11
			bottomRight.SetRGBA(x, y, pixel)
		}
	}
	images := []image.Image{
		scene.SubImage(image.Rect(0, 0, 400, 320)),
		scene.SubImage(image.Rect(400, 0, 800, 320)),
		scene.SubImage(image.Rect(0, 320, 400, 640)),
		bottomRight,
	}
	if artSeamMatches(images[1], images[3], false) {
		t.Fatal("fixture must have a rejected top-right/bottom-right seam")
	}
	got := joinArtPanels(images)
	if len(got) != 2 || got[0].last != 1 || got[1].first != 2 {
		t.Fatalf("a good seam concealed a rejected seam in the other column: %+v", got)
	}
}

func TestJoinArtGridRejectsAmbiguousBoundaries(t *testing.T) {
	panel := image.NewRGBA(image.Rect(0, 0, 320, 320))
	for i := 3; i < 317; i++ {
		c := color.RGBA{R: uint8(120 + 100*math.Sin(float64(i)*0.1)), A: 255}
		for depth := range 3 {
			panel.SetRGBA(depth, i, c)
			panel.SetRGBA(319-depth, i, c)
			panel.SetRGBA(i, depth, c)
			panel.SetRGBA(i, 319-depth, c)
		}
	}
	if !artSeamMatches(panel, panel, true) || !artSeamMatches(panel, panel, false) {
		t.Fatal("fixture must match in both orientations")
	}
	got := joinArtGrid([]image.Image{panel, panel})
	if got != nil {
		t.Fatal("ambiguous orientation was merged")
	}
}

func TestJoinArtGridRejectsSharedPatternedBorders(t *testing.T) {
	bounds := image.Rect(0, 0, 200, 320)
	a, b := artScene(bounds, 0), artScene(bounds, 2)
	for y := range bounds.Dy() {
		pixel := color.RGBA{R: uint8(120 + 100*math.Sin(float64(y)*0.1)), A: 255}
		for depth := range 3 {
			for _, panel := range []*image.RGBA{a, b} {
				panel.SetRGBA(depth, y, pixel)
				panel.SetRGBA(bounds.Max.X-1-depth, y, pixel)
			}
		}
	}
	if !artSeamMatches(a, b, true) || !artSeamMatches(b, a, true) {
		t.Fatal("fixture must have matching structured borders in both directions")
	}
	got := joinArtGrid([]image.Image{a, b})
	if got != nil {
		t.Fatal("unrelated pictures sharing a decorative border were merged")
	}
}

func TestJoinArtGridRejectsExtremeAspectRatio(t *testing.T) {
	// Uniform resizing cannot make an extreme aspect ratio deliverable.
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	a := image.NewRGBA(image.Rect(0, 0, 4900, 320))
	b := image.NewRGBA(image.Rect(0, 0, 4900, 320))
	draw.Draw(a, image.Rect(4897, 0, 4900, 320), scene, image.Pt(197, 0), draw.Src)
	draw.Draw(b, image.Rect(0, 0, 3, 320), scene, image.Pt(200, 0), draw.Src)
	if !artSeamMatches(a, b, true) {
		t.Fatal("fixture must have a matching seam")
	}
	got := joinArtGrid([]image.Image{a, b})
	if got != nil {
		t.Fatal("joined photo exceeds Telegram's aspect ratio limit")
	}
}
