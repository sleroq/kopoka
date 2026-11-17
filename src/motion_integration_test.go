package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func motionTestTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
}

func motionTestGIF(t *testing.T, delay int) []byte {
	t.Helper()
	palette := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{G: 255, A: 255}}
	anim := &gif.GIF{LoopCount: 0}
	for i := range 2 {
		img := image.NewPaletted(image.Rect(0, 0, 32, 32), palette)
		for p := range img.Pix {
			img.Pix[p] = uint8(i)
		}
		anim.Image = append(anim.Image, img)
		anim.Delay = append(anim.Delay, delay)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, anim); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func motionTestPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func motionTestFrame(t *testing.T, data []byte, at string) image.Image {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-v", "error", "-ss", at, "-i", path,
		"-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func motionAssertColor(t *testing.T, img image.Image, x, y int, want byte) {
	t.Helper()
	r, g, b, _ := img.At(x, y).RGBA()
	channels := []uint32{r, g, b}
	if channels[want] < 40000 {
		t.Fatalf("pixel %d,%d: %v", x, y, channels)
	}
}

func TestMotionMergeOrderAndLoopIntegration(t *testing.T) {
	motionTestTools(t)
	files := map[string][]byte{
		"/blue":  motionTestPNG(t, color.RGBA{B: 255, A: 255}),
		"/red":   motionTestPNG(t, color.RGBA{R: 255, A: 255}),
		"/short": motionTestGIF(t, 20), "/long": motionTestGIF(t, 60),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(files[r.URL.Path])
	}))
	defer server.Close()
	media := []xMedia{
		{URL: server.URL + "/blue"}, {Type: "gif", URL: server.URL + "/short"},
		{URL: server.URL + "/red"}, {Type: "gif", URL: server.URL + "/long"},
	}

	result, err := mergeMotionMedia(media, false)
	if err != nil {
		t.Fatal(err)
	}
	animation, ok := result.(tgbotapi.InputMediaAnimation)
	if !ok {
		t.Fatalf("got %T", result)
	}
	file := animation.Media.(tgbotapi.FileBytes)
	img := motionTestFrame(t, file.Bytes, "0.9")
	motionAssertColor(t, img, 16, 16, 2)
	motionAssertColor(t, img, 48, 16, 0)
	motionAssertColor(t, img, 80, 16, 0)
	motionAssertColor(t, img, 112, 16, 1)
}

func TestMotionFirstVideoIntegration(t *testing.T) {
	motionTestTools(t)
	dir := t.TempDir()
	makeVideo := func(name, source, duration string, audio bool) []byte {
		path := filepath.Join(dir, name+".mp4")
		args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", source}
		if audio {
			args = append(args, "-f", "lavfi", "-i", "sine=frequency=440")
		}
		args = append(args, "-t", duration, "-c:v", "libx264", "-pix_fmt", "yuv420p")
		if audio {
			args = append(args, "-c:a", "aac")
		}
		args = append(args, path)
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	first := makeVideo("first", "color=blue:s=32x32:r=10", "1.2", true)
	second := makeVideo("second", "testsrc=s=32x32:r=10", "2", false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			_, _ = w.Write(first)
		} else {
			_, _ = w.Write(second)
		}
	}))
	defer server.Close()
	media := []xMedia{
		{Type: "video", URL: server.URL + "/first"},
		{Type: "video", URL: server.URL + "/second"},
	}

	result, err := mergeMotionMedia(media, false)
	if err != nil {
		t.Fatal(err)
	}
	video, ok := result.(tgbotapi.InputMediaVideo)
	if !ok {
		t.Fatalf("got %T", result)
	}
	data := video.Media.(tgbotapi.FileBytes).Bytes

	path := filepath.Join(dir, "result.mp4")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	probe, err := motionProbe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if probe.duration < 1.1 || probe.duration > 1.3 || !probe.audio {
		t.Fatalf("duration/audio: %+v", probe)
	}

	a, b := motionTestFrame(t, data, "0.1"), motionTestFrame(t, data, "0.9")
	for _, p := range []image.Point{{40, 8}, {48, 16}, {56, 24}} {
		ar, ag, ab, _ := a.At(p.X, p.Y).RGBA()
		br, bg, bb, _ := b.At(p.X, p.Y).RGBA()
		if fmt.Sprint(ar, ag, ab) != fmt.Sprint(br, bg, bb) {
			t.Fatalf("later video changed at %v", p)
		}
	}
}

func TestMotionSceneIntegration(t *testing.T) {
	motionTestTools(t)
	data := motionTestGIF(t, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	scene := motionScene{
		Image: motionTestPNG(t, color.RGBA{B: 255, A: 255}),
		Slots: []motionSlot{{
			URL: server.URL + "/missing", FallbackURLs: []string{server.URL},
			Kind: "gif", X: 4, Y: 4, Width: 24, Height: 24, Radius: 10,
		}},
	}
	file, kind, err := renderMotion(scene)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "animation" {
		t.Fatalf("got kind %q", kind)
	}
	img := motionTestFrame(t, file.Bytes, "0.1")
	motionAssertColor(t, img, 16, 16, 0)
	motionAssertColor(t, img, 4, 4, 2)

	scene.Slots[0] = motionSlot{URL: server.URL, Kind: "gif", X: 5, Y: 5, Width: 22, Height: 22}
	file, _, err = renderMotion(scene)
	if err != nil {
		t.Fatal(err)
	}
	img = motionTestFrame(t, file.Bytes, "0.1")
	if luma := color.GrayModel.Convert(img.At(16, 4)).(color.Gray).Y; luma > 50 {
		t.Fatalf("motion overwrote the background row above odd-positioned slot: luma=%d", luma)
	}
}
