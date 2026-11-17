package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/image/draw"
	"golang.org/x/image/vector"
)

type motionSlot struct {
	URL          string   `json:"url"`
	FallbackURLs []string `json:"fallback_urls,omitempty"`
	Kind         string   `json:"kind"`
	X            int      `json:"x"`
	Y            int      `json:"y"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	Radius       int      `json:"radius"`
}
type motionScene struct {
	Image []byte       `json:"image"`
	Slots []motionSlot `json:"slots"`
}

const motionMaxBytes = 50_000_000

func hasMovingMedia(media []xMedia) bool {
	for _, item := range media {
		if item.Type == "gif" || item.Type == "video" {
			return true
		}
	}
	return false
}

type motionSource struct {
	path          string
	duration      float64
	width, height int
	audio         bool
}

func motionCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func motionDownload(client *http.Client, urls []string, path string) error {
	var last error
	for _, url := range urls {
		resp, err := client.Get(url)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			last = fmt.Errorf("download %s: HTTP %s", url, resp.Status)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, motionMaxBytes+1))
		_ = resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if len(data) > motionMaxBytes {
			last = fmt.Errorf("download %s exceeds 50 MB", url)
			continue
		}
		return os.WriteFile(path, data, 0600)
	}
	return fmt.Errorf("download motion media: %w", last)
}

func motionProbe(ctx context.Context, path string) (motionSource, error) {
	out, err := motionCommand(ctx, "ffprobe", "-v", "error", "-show_entries",
		"format=duration:stream=codec_type,width,height,duration", "-of", "json", path)
	if err != nil {
		return motionSource{}, err
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Type     string `json:"codec_type"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Duration string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return motionSource{}, err
	}
	s := motionSource{path: path}
	s.duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	for _, stream := range probe.Streams {
		if stream.Type == "video" && s.width == 0 {
			s.width, s.height = stream.Width, stream.Height
			if videoDuration, err := strconv.ParseFloat(stream.Duration, 64); err == nil && videoDuration > 0 {
				s.duration = videoDuration
			}
		}
		if stream.Type == "audio" {
			s.audio = true
		}
	}
	if s.width <= 0 || s.height <= 0 || s.duration <= 0 {
		return s, fmt.Errorf("invalid motion media dimensions or duration: %s", path)
	}
	return s, nil
}

func motionMask(width, height, radius int) *image.Alpha {
	mask := image.NewAlpha(image.Rect(0, 0, width, height))
	r := float32(min(max(radius, 0), min(width, height)/2))
	w, h := float32(width), float32(height)
	z := vector.NewRasterizer(width, height)
	z.MoveTo(r, 0)
	z.LineTo(w-r, 0)
	z.QuadTo(w, 0, w, r)
	z.LineTo(w, h-r)
	z.QuadTo(w, h, w-r, h)
	z.LineTo(r, h)
	z.QuadTo(0, h, 0, h-r)
	z.LineTo(0, r)
	z.QuadTo(0, 0, r, 0)
	z.ClosePath()
	z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	return mask
}

func renderMotion(scene motionScene) (tgbotapi.FileBytes, string, error) {
	dir, err := os.MkdirTemp("", "kopoka-motion-*")
	if err != nil {
		return tgbotapi.FileBytes{}, "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	sources := make([]motionSource, len(scene.Slots))
	for i, slot := range scene.Slots {
		path := filepath.Join(dir, fmt.Sprintf("source-%d", i))
		if err := motionDownload(client, append([]string{slot.URL}, slot.FallbackURLs...), path); err != nil {
			return tgbotapi.FileBytes{}, "", err
		}
		sources[i], err = motionProbe(ctx, path)
		if err != nil {
			return tgbotapi.FileBytes{}, "", err
		}
	}
	return renderMotionFiles(ctx, dir, scene, sources)
}

func renderMotionFiles(ctx context.Context, dir string, scene motionScene, sources []motionSource) (tgbotapi.FileBytes, string, error) {
	var empty tgbotapi.FileBytes
	config, _, err := image.DecodeConfig(bytes.NewReader(scene.Image))
	if err != nil {
		return empty, "", fmt.Errorf("decode scene: %w", err)
	}
	if len(sources) == 0 {
		return empty, "", fmt.Errorf("motion scene has no slots")
	}

	poster := filepath.Join(dir, "scene.png")
	if err := os.WriteFile(poster, scene.Image, 0600); err != nil {
		return empty, "", err
	}

	duration := 0.0
	firstVideo := -1
	for i, slot := range scene.Slots {
		if slot.Width <= 0 || slot.Height <= 0 || slot.X < 0 || slot.Y < 0 || slot.X+slot.Width > config.Width || slot.Y+slot.Height > config.Height {
			return empty, "", fmt.Errorf("invalid motion slot %d", i)
		}
		if slot.Kind == "video" && firstVideo < 0 {
			firstVideo = i
		}
		duration = max(duration, sources[i].duration)
	}
	kind := "animation"
	if firstVideo >= 0 {
		kind = "video"
		duration = sources[firstVideo].duration
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-loop", "1", "-framerate", "30", "-i", poster}
	for i, slot := range scene.Slots {
		if slot.Kind == "gif" {
			args = append(args, "-stream_loop", "-1")
		}
		args = append(args, "-i", sources[i].path)
	}

	filters := []string{"[0:v]format=rgba[base0]"}
	for i, slot := range scene.Slots {
		maskPath := filepath.Join(dir, fmt.Sprintf("mask-%d.png", i))
		f, err := os.Create(maskPath)
		if err != nil {
			return empty, "", err
		}
		err = png.Encode(f, motionMask(slot.Width, slot.Height, slot.Radius))
		closeErr := f.Close()
		if err != nil {
			return empty, "", err
		}
		if closeErr != nil {
			return empty, "", closeErr
		}
		args = append(args, "-loop", "1", "-framerate", "30", "-i", maskPath)
		prefix := ""
		if slot.Kind == "video" && i != firstVideo {
			prefix = fmt.Sprintf("trim=end_frame=1,setpts=PTS-STARTPTS,tpad=stop_mode=clone:stop_duration=%.3f,", duration)
		}
		tileFilter := fmt.Sprintf("[%d:v]%sscale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1,fps=30,format=rgba[tile%d]",
			i+1, prefix, slot.Width, slot.Height, slot.Width, slot.Height, i)
		filters = append(filters, tileFilter)
		overlayFilter := fmt.Sprintf("[%d:v]format=rgba,alphaextract[mask%d];[tile%d][mask%d]alphamerge[masked%d];[base%d][masked%d]overlay=%d:%d:shortest=1:format=rgb[base%d]",
			len(sources)+1+i, i, i, i, i, i, i, slot.X, slot.Y, i+1)
		filters = append(filters, overlayFilter)
	}
	outputFilter := fmt.Sprintf("[base%d]scale=w='min(1920,iw)':h='min(1920,ih)':"+
		"force_original_aspect_ratio=decrease:force_divisible_by=2,format=yuv420p[out]", len(sources))
	filters = append(filters, outputFilter)
	output := filepath.Join(dir, "output.mp4")
	args = append(args, "-filter_complex_threads", "1", "-filter_complex", strings.Join(filters, ";"), "-map", "[out]")
	if firstVideo >= 0 && sources[firstVideo].audio {
		args = append(args, "-map", fmt.Sprintf("%d:a:0", firstVideo+1), "-c:a", "aac")
	}
	args = append(args, "-t", strconv.FormatFloat(duration, 'f', 6, 64), "-r", "30",
		"-c:v", "libx264", "-preset", "medium", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart", "-fs", strconv.Itoa(motionMaxBytes+1), output)
	if _, err := motionCommand(ctx, "ffmpeg", args...); err != nil {
		return empty, "", err
	}
	rendered, err := motionProbe(ctx, output)
	if err != nil {
		return empty, "", err
	}
	if rendered.duration+0.1 < duration {
		return empty, "", fmt.Errorf("motion output stopped before its full duration (possibly exceeded 50 MB)")
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return empty, "", err
	}
	if len(data) > motionMaxBytes {
		return empty, "", fmt.Errorf("motion output exceeds 50 MB")
	}
	return tgbotapi.FileBytes{Name: "motion.mp4", Bytes: data}, kind, nil
}

func mergeMotionMedia(media []xMedia, bars bool) (any, error) {
	dir, err := os.MkdirTemp("", "kopoka-motion-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	images, sources, height, err := loadMotionPanels(ctx, dir, media)
	if err != nil {
		return nil, err
	}
	var slots []motionSlot
	if len(media) == 0 {
		return nil, fmt.Errorf("empty motion media")
	}

	widths := make([]int, len(media))
	total := 0
	for i, img := range images {
		widths[i] = max(1, int(math.Round(float64(img.Bounds().Dx())*float64(height)/float64(img.Bounds().Dy()))))
		total += widths[i]
	}
	gap := 0
	if bars {
		gap = max(1, height*8/905)
		total += gap * (len(media) - 1)
	}
	scale := math.Min(1, 1920/float64(max(total, height)))
	height = max(1, int(float64(height)*scale))
	gap = int(float64(gap) * scale)
	total = gap * (len(media) - 1)
	for i := range widths {
		widths[i] = max(1, int(float64(widths[i])*scale))
		total += widths[i]
	}

	canvas := image.NewRGBA(image.Rect(0, 0, total, height))
	background := color.White
	if bars {
		background = color.Black
	}
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
	x := 0
	for i, img := range images {
		rect := image.Rect(x, 0, x+widths[i], height)
		panel := image.NewRGBA(image.Rect(0, 0, widths[i], height))
		draw.CatmullRom.Scale(panel, panel.Bounds(), img, img.Bounds(), draw.Src, nil)
		radius := 0
		if bars {
			radius = max(1, height*12/905)
			draw.DrawMask(canvas, rect, panel, image.Point{}, motionMask(widths[i], height, radius), image.Point{}, draw.Over)
		} else {
			draw.Draw(canvas, rect, panel, image.Point{}, draw.Src)
		}
		if media[i].Type != "" && media[i].Type != "photo" {
			slot := motionSlot{URL: media[i].URL, Kind: media[i].Type, X: x, Width: widths[i], Height: height, Radius: radius}
			slots = append(slots, slot)
		}
		x += widths[i] + gap
	}

	var poster bytes.Buffer
	if err := png.Encode(&poster, canvas); err != nil {
		return nil, err
	}
	file, kind, err := renderMotionFiles(ctx, dir, motionScene{Image: poster.Bytes(), Slots: slots}, sources)
	if err != nil {
		return nil, err
	}
	if kind == "video" {
		return tgbotapi.NewInputMediaVideo(file), nil
	}
	return tgbotapi.NewInputMediaAnimation(file), nil
}

func loadMotionPanels(ctx context.Context, dir string, media []xMedia) ([]image.Image, []motionSource, int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	images := make([]image.Image, len(media))
	sources := make([]motionSource, 0, len(media))
	height := 0

	for i, item := range media {
		path := filepath.Join(dir, fmt.Sprintf("input-%d", i))
		if err := motionDownload(client, append([]string{item.URL}, item.FallbackURLs...), path); err != nil {
			return nil, nil, 0, err
		}
		if item.Type == "" || item.Type == "photo" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, nil, 0, err
			}
			images[i], _, err = image.Decode(bytes.NewReader(data))
			if err != nil {
				return nil, nil, 0, fmt.Errorf("decode photo: %w", err)
			}
		} else {
			source, err := motionProbe(ctx, path)
			if err != nil {
				return nil, nil, 0, err
			}
			frame := filepath.Join(dir, fmt.Sprintf("frame-%d.png", i))
			if _, err := motionCommand(ctx, "ffmpeg", "-v", "error", "-y", "-i", path, "-frames:v", "1", frame); err != nil {
				return nil, nil, 0, err
			}
			data, err := os.ReadFile(frame)
			if err != nil {
				return nil, nil, 0, err
			}
			images[i], _, err = image.Decode(bytes.NewReader(data))
			if err != nil {
				return nil, nil, 0, err
			}
			sources = append(sources, source)
		}
		height = max(height, images[i].Bounds().Dy())
	}
	return images, sources, height, nil
}
