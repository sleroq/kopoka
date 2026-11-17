package main

import (
	"bytes"
	"fmt"
	"html"
	"image"
	"image/draw"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const originalFileLimit = 50 * 1024 * 1024

type originalSource struct {
	url, preview, kind string
	index              int
	file               tgbotapi.FileBytes
	upload             tgbotapi.FilePath
	joined             []originalSource
}

type originalFailures []string

func (failures *originalFailures) add(source originalSource, err error) {
	if len(source.joined) > 0 {
		for _, part := range source.joined {
			failures.add(part, err)
		}
		return
	}
	log.Printf("sending original file %d (%s): %v", source.index, source.url, err)
	ordinal := originalOrdinal(source.index) + " file"
	if source.url != "" {
		ordinal = `<a href="` + html.EscapeString(source.url) + `">` + ordinal + `</a>`
	}
	*failures = append(*failures, "failed files: unable to upload "+ordinal+" for you")
}

func (failures originalFailures) report(bot *tgbotapi.BotAPI, chatID int64, replyID int) error {
	if len(failures) == 0 {
		return nil
	}
	msg := tgbotapi.NewMessage(chatID, strings.Join(failures, "\n"))
	msg.ReplyToMessageID = replyID
	msg.ParseMode = "HTML"
	if _, err := retryTelegram(func() (tgbotapi.Message, error) { return bot.Send(msg) }); err != nil {
		return fmt.Errorf("sending original file failure report: %w", err)
	}
	return nil
}

func sendOriginalFiles(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, chatID int64, replyID int, link *url.URL, saved *artwork) error {
	sources, pixivFiles, err := originalFileURLs(app, link)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("artwork has no original files")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var failures originalFailures
	if saved == nil || len(saved.Photos) < len(sources) {
		sources = originalPanels(client, sources, pixivFiles, failures.add)
	}

	var ready []originalSource
	for _, source := range sources {
		if source.url == "" && source.file.Bytes == nil {
			failures.add(source, fmt.Errorf("missing source URL"))
			continue
		}
		if pixivFiles && source.file.Bytes == nil {
			source.file, err = downloadOriginal(client, source.url, true)
			if err != nil {
				failures.add(source, err)
				continue
			}
		}
		ready = append(ready, source)
	}

	for len(ready) > 0 {
		n := min(10, len(ready))
		if len(ready)-n == 1 {
			n--
		}
		sendOriginalBatch(bot, client, chatID, replyID, ready[:n], pixivFiles, failures.add)
		ready = ready[n:]
	}
	return failures.report(bot, chatID, replyID)
}

func originalFileURLs(app *pixiv.AppPixivAPI, link *url.URL) ([]originalSource, bool, error) {
	if link.Hostname() == "www.pixiv.net" || link.Hostname() == "pixiv.net" {
		sources, err := originalPixivFiles(app, link)
		return sources, true, err
	}

	post, err := fetchXPost(link, true)
	if err != nil {
		return nil, false, err
	}
	_, index := xMediaSelection(link)
	if err := selectXMedia(&post, link); err != nil {
		return nil, false, err
	}
	sources := make([]originalSource, 0, len(post.Media))
	for i, media := range post.Media {
		raw, preview, kind := media.URL, "", media.Type
		if kind == "" {
			kind = "photo"
		}
		if kind == "photo" {
			preview = xPhotoURL(raw)
			u, err := url.Parse(raw)
			if err == nil && u.Hostname() == "pbs.twimg.com" && strings.HasPrefix(u.Path, "/media/") {
				q := u.Query()
				q.Set("name", "orig")
				u.RawQuery = q.Encode()
				raw = u.String()
			}
		}
		ordinal := i + 1
		if index > 0 {
			ordinal = index
		}
		source := originalSource{url: raw, preview: preview, kind: kind, index: ordinal}
		sources = append(sources, source)
	}
	return sources, false, nil
}

func originalPanels(client *http.Client, sources []originalSource, pixivFiles bool, fail func(originalSource, error)) []originalSource {
	var result []originalSource
	for start := 0; start < len(sources); {
		if sources[start].kind != "photo" {
			result = append(result, sources[start])
			start++
			continue
		}
		end := start + 1
		for end < len(sources) && sources[end].kind == "photo" {
			end++
		}
		if end-start == 1 {
			result = append(result, sources[start])
		} else {
			result = append(result, originalPhotoRun(client, sources[start:end], pixivFiles, fail)...)
		}
		start = end
	}
	return result
}

func originalPhotoRun(client *http.Client, sources []originalSource, pixivFiles bool, fail func(originalSource, error)) []originalSource {
	images := make([]image.Image, 0, len(sources))
	for _, source := range sources {
		var file tgbotapi.RequestFileData = tgbotapi.FileURL(source.preview)
		if pixivFiles {
			photo, err := downloadPixivPhoto(client, source.preview, source.url)
			if err != nil {
				return sources
			}
			file = photo
		}
		img, err := downloadArtPhoto(client, file)
		if err != nil {
			return sources
		}
		images = append(images, img)
	}

	panels := joinArtPanels(images)
	merged := make([]bool, len(panels))
	for i, panel := range panels {
		if panel.first == panel.last {
			continue
		}
		file, err := mergedPhotoFile(panel.image)
		if err != nil {
			return sources
		}
		merged[i] = file.Bytes != nil
	}

	var result []originalSource
	for i, panel := range panels {
		parts := sources[panel.first : panel.last+1]
		if !merged[i] {
			result = append(result, parts...)
			continue
		}
		previews := images[panel.first : panel.last+1]
		joined, err := joinOriginalPanel(client, parts, previews, pixivFiles)
		if err != nil {
			fail(joined, err)
		} else {
			result = append(result, joined)
		}
	}
	return result
}

func joinOriginalPanel(client *http.Client, sources []originalSource, previews []image.Image, pixivFiles bool) (originalSource, error) {
	joined := originalSource{index: sources[0].index}
	for _, source := range sources {
		joined.joined = append(joined.joined, originalSource{url: source.url, index: source.index})
	}

	var images []image.Image
	for _, source := range sources {
		file, err := downloadOriginal(client, source.url, pixivFiles)
		if err != nil {
			return joined, err
		}
		img, _, err := image.Decode(bytes.NewReader(file.Bytes))
		if err != nil {
			return joined, fmt.Errorf("decoding original panel: %w", err)
		}
		images = append(images, img)
	}

	columns := 0
	for candidate := 1; candidate <= len(previews); candidate++ {
		if len(previews)%candidate == 0 {
			if _, matches := artGridMatches(previews, candidate); matches {
				columns = candidate
				break
			}
		}
	}
	if columns == 0 {
		return joined, fmt.Errorf("missing preview panel layout")
	}
	size, fits := artGridSize(images, columns)
	if !fits {
		return joined, fmt.Errorf("original panel dimensions do not match")
	}
	canvas := image.NewNRGBA(image.Rectangle{Max: size})
	drawArtGrid(canvas, images, columns, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		return joined, err
	}
	if buf.Len() > originalFileLimit {
		return joined, fmt.Errorf("joined file exceeds 50 MiB")
	}
	joined.file = tgbotapi.FileBytes{Name: "joined-art.png", Bytes: buf.Bytes()}
	return joined, nil
}

func (source originalSource) telegramFile() tgbotapi.RequestFileData {
	if source.file.Bytes != nil {
		return source.file
	}
	if source.upload != "" {
		return source.upload
	}
	return tgbotapi.FileURL(source.url)
}

func uploadOriginalBatch(bot *tgbotapi.BotAPI, chatID int64, replyID int, sources []originalSource) error {
	if len(sources) == 1 {
		doc := tgbotapi.NewDocument(chatID, sources[0].telegramFile())
		doc.ReplyToMessageID = replyID
		_, err := retryTelegram(func() (tgbotapi.Message, error) { return bot.Send(doc) })
		return err
	}
	media := make([]any, 0, len(sources))
	for _, source := range sources {
		media = append(media, tgbotapi.NewInputMediaDocument(source.telegramFile()))
	}
	group := tgbotapi.NewMediaGroup(chatID, media)
	group.ReplyToMessageID = replyID
	_, err := retryTelegram(func() ([]tgbotapi.Message, error) { return bot.SendMediaGroup(group) })
	return err
}

func sendOriginalBatch(bot *tgbotapi.BotAPI, client *http.Client, chatID int64, replyID int, sources []originalSource, pixivFiles bool, fail func(originalSource, error)) {
	err := uploadOriginalBatch(bot, chatID, replyID, sources)
	if err == nil {
		return
	}

	var downloaded []originalSource
	var temporary []tgbotapi.FilePath
	defer func() {
		for _, file := range temporary {
			_ = os.Remove(string(file))
		}
	}()
	hadURLs := false
	for _, source := range sources {
		if source.file.Bytes == nil && source.upload == "" {
			hadURLs = true
			if source.kind == "video" || source.kind == "gif" {
				source.upload, err = downloadVideo(source.url)
				if err == nil {
					temporary = append(temporary, source.upload)
				}
			} else {
				source.file, err = downloadOriginal(client, source.url, pixivFiles)
			}
			if err != nil {
				fail(source, err)
				continue
			}
		}
		downloaded = append(downloaded, source)
	}
	if hadURLs {
		if len(downloaded) > 0 {
			sendOriginalBatch(bot, client, chatID, replyID, downloaded, pixivFiles, fail)
		}
		return
	}
	if len(sources) == 1 {
		fail(sources[0], err)
		return
	}
	middle := len(sources) / 2
	sendOriginalBatch(bot, client, chatID, replyID, sources[:middle], pixivFiles, fail)
	sendOriginalBatch(bot, client, chatID, replyID, sources[middle:], pixivFiles, fail)
}

func originalPixivFiles(app *pixiv.AppPixivAPI, link *url.URL) ([]originalSource, error) {
	id, err := strconv.ParseUint(strings.TrimPrefix(link.Path, "/artworks/"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing Pixiv ID: %w", err)
	}

	pixivRequestMu.Lock()
	illust, err := app.IllustDetail(id)
	pixivRequestMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("fetching Pixiv illust: %w", err)
	}
	selected, err := selectPixivPhoto(illust, link)
	if err != nil {
		return nil, err
	}

	if illust.Type == "ugoira" {
		pixivRequestMu.Lock()
		metadata, err := app.UgoiraMetadata(id)
		pixivRequestMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("fetching Pixiv ugoira metadata: %w", err)
		}
		raw := metadata.UgoiraMetadataUgoiraMetadata.ZipURLs.Medium
		raw = strings.Replace(raw, "_ugoira600x600.zip", "_ugoira1920x1080.zip", 1)
		return []originalSource{{url: raw, kind: "ugoira", index: 1}}, nil
	}
	if len(illust.MetaPages) == 0 {
		source := originalSource{
			url: illust.MetaSinglePage.OriginalImageURL, preview: illust.Images.Large,
			kind: "photo", index: 1,
		}
		return []originalSource{source}, nil
	}
	sources := make([]originalSource, 0, len(illust.MetaPages))
	for i, page := range illust.MetaPages {
		if selected > 0 && i != selected-1 {
			continue
		}
		source := originalSource{
			url: page.Images.Original, preview: page.Images.Large,
			kind: "photo", index: i + 1,
		}
		sources = append(sources, source)
	}
	return sources, nil
}

func downloadOriginal(client *http.Client, raw string, pixivFile bool) (tgbotapi.FileBytes, error) {
	var file tgbotapi.FileBytes
	if raw == "" {
		return file, fmt.Errorf("missing source URL")
	}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return file, err
	}
	if pixivFile {
		req.Header.Set("Referer", "https://www.pixiv.net/")
	}
	res, err := client.Do(req)
	if err != nil {
		return file, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return file, fmt.Errorf("download: HTTP %s", res.Status)
	}
	if res.ContentLength > originalFileLimit {
		return file, fmt.Errorf("file exceeds 50 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, originalFileLimit+1))
	if err != nil {
		return file, fmt.Errorf("download: %w", err)
	}
	if len(data) > originalFileLimit {
		return file, fmt.Errorf("file exceeds 50 MiB")
	}
	return tgbotapi.FileBytes{Name: path.Base(req.URL.Path), Bytes: data}, nil
}

func originalOrdinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}
