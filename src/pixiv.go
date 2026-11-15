package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func pixivPhotoIndex(link *url.URL) int {
	if link.Host != "pixiv.net" && link.Host != "www.pixiv.net" {
		return 0
	}
	index, _ := strconv.Atoi(link.Query().Get("photo"))
	return index
}

func selectPixivPhoto(illust *pixiv.Illust, link *url.URL) (int, error) {
	selected := pixivPhotoIndex(link)
	count := len(illust.MetaPages)
	if illust.Type == "ugoira" || count == 0 {
		count = 1
	}
	if selected > count {
		return 0, unavailablePhotoError{selected}
	}
	return selected, nil
}

func downloadPixivPhoto(client *http.Client, preferredURL, fallbackURL string) (tgbotapi.FileBytes, error) {
	// Upload locally: Telegram cannot fetch Pixiv's hotlink-protected URLs.
	const maxPhotoSize = 10 * 1024 * 1024
	for _, imageURL := range []string{preferredURL, fallbackURL} {
		if imageURL == "" {
			continue
		}
		file, err := downloadArtFile(client, tgbotapi.FileURL(imageURL))
		if err != nil {
			return file, fmt.Errorf("downloading Pixiv image: %w", err)
		}
		if len(file.Bytes) <= maxPhotoSize {
			return file, nil
		}
	}
	return tgbotapi.FileBytes{}, fmt.Errorf("no Pixiv image variant fits Telegram's 10 MB photo limit")
}

func downloadPixivUgoira(client *http.Client, metadata *pixiv.UgoiraMetadataClass) (tgbotapi.FileBytes, error) {
	archive, err := downloadArtFile(client, tgbotapi.FileURL(metadata.ZipURLs.Medium))
	if err != nil {
		return tgbotapi.FileBytes{}, fmt.Errorf("downloading Pixiv ugoira: %w", err)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(archive.Bytes), int64(len(archive.Bytes)))
	if err != nil {
		return tgbotapi.FileBytes{}, fmt.Errorf("opening Pixiv ugoira ZIP: %w", err)
	}
	dir, err := os.MkdirTemp("", "kopoka-ugoira-*")
	if err != nil {
		return tgbotapi.FileBytes{}, fmt.Errorf("creating ugoira directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	entries := make(map[string]*zip.File, len(zipReader.File))
	for _, entry := range zipReader.File {
		entries[entry.Name] = entry
	}
	var list strings.Builder
	for i, frame := range metadata.Frames {
		entry, ok := entries[frame.File]
		if !ok || frame.Delay <= 0 {
			return tgbotapi.FileBytes{}, fmt.Errorf("invalid ugoira frame %q or delay %d", frame.File, frame.Delay)
		}
		input, err := entry.Open()
		if err != nil {
			return tgbotapi.FileBytes{}, fmt.Errorf("opening ugoira frame: %w", err)
		}
		data, readErr := io.ReadAll(input)
		closeErr := input.Close()
		if readErr != nil {
			return tgbotapi.FileBytes{}, fmt.Errorf("reading ugoira frame: %w", readErr)
		}
		if closeErr != nil {
			return tgbotapi.FileBytes{}, fmt.Errorf("closing ugoira frame: %w", closeErr)
		}
		name := fmt.Sprintf("%06d%s", i, filepath.Ext(frame.File))
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return tgbotapi.FileBytes{}, fmt.Errorf("writing ugoira frame: %w", err)
		}
		fmt.Fprintf(&list, "file '%s'\noption framerate 1000\nduration %.3f\n", name, float64(frame.Delay)/1000)
	}
	if len(metadata.Frames) == 0 {
		return tgbotapi.FileBytes{}, fmt.Errorf("ugoira has no frames")
	}
	// The concat demuxer needs a final timestamp to preserve the last delay.
	last := metadata.Frames[len(metadata.Frames)-1]
	fmt.Fprintf(&list, "file '%06d%s'\noption framerate 1000\n", len(metadata.Frames)-1, filepath.Ext(last.File))
	if err := os.WriteFile(filepath.Join(dir, "frames.txt"), []byte(list.String()), 0600); err != nil {
		return tgbotapi.FileBytes{}, fmt.Errorf("writing ugoira frame list: %w", err)
	}
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0", "-i", "frames.txt",
		"-fps_mode", "vfr", "-c:v", "libx264", "-bf", "0",
		"-pix_fmt", "yuv420p", "-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2",
		"-movflags", "+faststart", "output.mp4",
	)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		return tgbotapi.FileBytes{}, fmt.Errorf("converting Pixiv ugoira: %w: %s", err, output)
	}
	video, err := os.ReadFile(filepath.Join(dir, "output.mp4"))
	if err != nil {
		return tgbotapi.FileBytes{}, fmt.Errorf("reading Pixiv ugoira video: %w", err)
	}
	return tgbotapi.FileBytes{Name: "ugoira.mp4", Bytes: video}, nil
}

func setupPixivClient() (*pixiv.AppPixivAPI, error) {
	tokenDeadlineSec, err := strconv.Atoi(os.Getenv("PIXIV_TOKEN_DEADLINE"))
	if err != nil {
		return nil, fmt.Errorf("parsing PIXIV_TOKEN_DEADLINE: %w", err)
	}

	// The library's token refresh uses http.DefaultClient, even with WithClient.
	http.DefaultClient.Transport = newPixivTransport(http.DefaultTransport.(*http.Transport))

	tokenDeadline := time.Now().Add(time.Duration(tokenDeadlineSec) * time.Second)
	_, err = pixiv.LoadAuth(
		os.Getenv("PIXIV_ACCESS_TOKEN"),
		os.Getenv("PIXIV_REFRESH_TOKEN"),
		tokenDeadline,
	)
	if err != nil {
		return nil, fmt.Errorf("logging into Pixiv: %w", err)
	}

	return pixiv.NewApp(), nil
}

type pixivTransport struct {
	base  *http.Transport
	pixiv *http.Transport
}

func newPixivTransport(base *http.Transport) *pixivTransport {
	transport := base.Clone()
	// Pixiv rejects Go's HTTP/2 requests with an HTML 403 instead of JSON.
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &pixivTransport{base: base, pixiv: transport}
}

func (t *pixivTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Hostname() {
	case "oauth.secure.pixiv.net", "app-api.pixiv.net":
		return t.pixiv.RoundTrip(req)
	default:
		return t.base.RoundTrip(req)
	}
}
