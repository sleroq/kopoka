package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type photoThumbnailServer struct {
	mu               sync.Mutex
	panels           []image.Image
	url              string
	paths, downloads []string
}

func (s *photoThumbnailServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/p") {
		s.downloads = append(s.downloads, r.URL.Path)
		_ = png.Encode(w, s.panels[int(r.URL.Path[2]-'0')])
		return
	}
	s.paths = append(s.paths, r.URL.Path)
	if r.URL.Path != "/user/status/30" && r.URL.Path != "/user/status/31" && r.URL.Path != "/user/status/32" && r.URL.Path != "/user/status/33" {
		http.NotFound(w, r)
		return
	}
	items := []map[string]string{
		{"type": "photo", "url": s.url + "/p0"},
		{"url": s.url + "/p1"},
		{"type": "video", "thumbnail_url": s.url + "/p0"},
	}
	if r.URL.Path == "/user/status/31" {
		items = items[:1]
	}
	if r.URL.Path == "/user/status/32" {
		items = []map[string]string{items[0], {"type": "photo"}}
	}
	if r.URL.Path == "/user/status/33" {
		items = []map[string]string{items[0], {"type": "photo", "url": s.url + "/missing"}}
	}
	response := map[string]any{
		"code": 200, "tweet": map[string]any{"media": map[string]any{"all": items}},
	}
	_ = json.NewEncoder(w).Encode(response)
}

func TestSelectedPhotoThumbnailHTTP(t *testing.T) {
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	panels := []image.Image{scene.SubImage(image.Rect(0, 0, 200, 320)), scene.SubImage(image.Rect(200, 0, 400, 320))}
	api := &photoThumbnailServer{panels: panels}
	upstream := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	defer upstream.Close()
	api.url = upstream.URL
	previews := testPreviews(t, nil, nil)
	target, _ := url.Parse(upstream.URL)
	previews.client = &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	server := httptest.NewServer(previews)
	defer server.Close()

	get := func(link string) *http.Response {
		t.Helper()
		res, err := http.Get(server.URL + "/thumbnail?size=large&url=" + url.QueryEscape(link))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	parent := "https://x.com/user/status/30"
	whole := readThumbnail(t, get(parent))
	if whole.Bounds().Size() != image.Pt(400, 320) {
		t.Fatalf("whole post did not merge panels: %v", whole.Bounds().Size())
	}
	for i := range 2 {
		img := readThumbnail(t, get(fmt.Sprintf("%s/photo/%d", parent, i+1)))
		if img.Bounds().Size() != image.Pt(240, 384) {
			t.Fatalf("photo %d dimensions: %v", i+1, img.Bounds().Size())
		}
		// Compare a grid away from badges, allowing JPEG and scaling differences.
		for y := 80; y < 280; y += 40 {
			for x := 30; x < 180; x += 30 {
				want := panels[i].At(panels[i].Bounds().Min.X+x, y)
				got := img.At(x*240/200, y*384/320)
				wr, wg, wb, _ := want.RGBA()
				gr, gg, gb, _ := got.RGBA()
				if absColor(wr, gr)+absColor(wg, gg)+absColor(wb, gb) > 18000 {
					t.Fatalf("photo %d pixel (%d,%d) differs from source", i+1, x, y)
				}
			}
		}
		checkSelectedBadge(t, previews, img, panels[i], fmt.Sprintf("%d/3", i+1), fmt.Sprintf("%d/3", 2-i))
	}
	single := readThumbnail(t, get("https://x.com/user/status/31/photo/1"))
	checkSelectedBadge(t, previews, single, panels[0], "1/1", "1/3")
	for _, suffix := range []string{"/photo/3", "/photo/4"} {
		res := get(parent + suffix)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: status %s", suffix, res.Status)
		}
	}
	for _, broken := range []string{"https://x.com/user/status/32", "https://x.com/user/status/33"} {
		res := get(broken)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadGateway {
			t.Errorf("whole post with unavailable media: %s", res.Status)
		}
		_ = readThumbnail(t, get(broken+"/photo/1"))
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if fmt.Sprint(api.paths) != "[/user/status/30 /user/status/30 /user/status/30 /user/status/31 /user/status/30 /user/status/30 /user/status/32 /user/status/32 /user/status/33 /missing /user/status/33]" {
		t.Errorf("FxTwitter paths: %v", api.paths)
	}
	downloads := map[string]int{}
	for _, path := range api.downloads {
		downloads[path]++
	}
	if len(api.downloads) != 10 || downloads["/p0"] != 8 || downloads["/p1"] != 2 {
		t.Errorf("image downloads: %v", api.downloads)
	}
}

func absColor(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func checkSelectedBadge(t *testing.T, previews *thumbnails, img, source image.Image, label, alternative string) {
	t.Helper()

	left := checkBadgeLabel(t, previews, img, label, alternative)
	size := img.Bounds().Size()
	scale := float64(min(size.X, size.Y)) / 384
	// The area immediately left of the count badge must contain the source, not a merge badge.
	x := size.X - int((384-float64(left/4-20))*scale)
	y := int(34 * scale)
	sizeSource := source.Bounds().Size()
	want := source.At(source.Bounds().Min.X+x*sizeSource.X/size.X, source.Bounds().Min.Y+y*sizeSource.Y/size.Y)
	wr, wg, wb, _ := want.RGBA()
	gr, gg, gb, _ := img.At(x, y).RGBA()
	if absColor(wr, gr)+absColor(wg, gg)+absColor(wb, gb) > 18000 {
		t.Errorf("%s: unexpected merge badge", label)
	}
}
