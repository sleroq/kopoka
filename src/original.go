package main

import (
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const originalFileLimit = 50 * 1024 * 1024

func sendOriginalFiles(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, chatID int64, replyID int, link *url.URL) error {
	files, pixivFiles, err := originalFileURLs(app, link)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("artwork has no original files")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var failures []string
	for i, fileURL := range files {
		err := sendOriginalFile(bot, client, chatID, replyID, fileURL, pixivFiles)
		if err == nil {
			continue
		}
		log.Printf("sending original file %d (%s): %v", i+1, fileURL, err)
		ordinal := originalOrdinal(i+1) + " file"
		if fileURL != "" {
			ordinal = `<a href="` + html.EscapeString(fileURL) + `">` + ordinal + `</a>`
		}
		failures = append(failures, "failed files: unable to upload "+ordinal+" for you")
	}
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

func originalFileURLs(app *pixiv.AppPixivAPI, link *url.URL) ([]string, bool, error) {
	if link.Hostname() != "www.pixiv.net" && link.Hostname() != "pixiv.net" {
		post, err := fetchXPost(link, true)
		if err != nil {
			return nil, false, err
		}

		files := make([]string, 0, len(post.Media))
		for _, media := range post.Media {
			raw := media.URL
			if media.Type == "" || media.Type == "photo" {
				u, err := url.Parse(raw)
				if err == nil && u.Hostname() == "pbs.twimg.com" && strings.HasPrefix(u.Path, "/media/") {
					q := u.Query()
					q.Set("name", "orig")
					u.RawQuery = q.Encode()
					raw = u.String()
				}
			}
			files = append(files, raw)
		}

		return files, false, nil
	}
	files, err := originalPixivFiles(app, link)
	return files, true, err
}

func originalPixivFiles(app *pixiv.AppPixivAPI, link *url.URL) ([]string, error) {
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
	if illust.Type == "ugoira" {
		pixivRequestMu.Lock()
		metadata, err := app.UgoiraMetadata(id)
		pixivRequestMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("fetching Pixiv ugoira metadata: %w", err)
		}
		raw := metadata.UgoiraMetadataUgoiraMetadata.ZipURLs.Medium
		raw = strings.Replace(raw, "_ugoira600x600.zip", "_ugoira1920x1080.zip", 1)
		return []string{raw}, nil
	}
	if len(illust.MetaPages) == 0 {
		return []string{illust.MetaSinglePage.OriginalImageURL}, nil
	}
	files := make([]string, 0, len(illust.MetaPages))
	for _, page := range illust.MetaPages {
		files = append(files, page.Images.Original)
	}
	return files, nil
}

func sendOriginalFile(bot *tgbotapi.BotAPI, client *http.Client, chatID int64, replyID int, raw string, pixivFile bool) error {
	if raw == "" {
		return fmt.Errorf("missing source URL")
	}

	send := func(file tgbotapi.RequestFileData) error {
		doc := tgbotapi.NewDocument(chatID, file)
		doc.ReplyToMessageID = replyID
		_, err := retryTelegram(func() (tgbotapi.Message, error) { return bot.Send(doc) })
		return err
	}

	if !pixivFile {
		if err := send(tgbotapi.FileURL(raw)); err == nil {
			return nil
		} else {
			log.Printf("Telegram URL upload failed for %s: %v; downloading", raw, err)
		}
	}

	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	if pixivFile {
		req.Header.Set("Referer", "https://www.pixiv.net/")
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %s", res.Status)
	}
	if res.ContentLength > originalFileLimit {
		return fmt.Errorf("file exceeds 50 MiB")
	}

	data, err := io.ReadAll(io.LimitReader(res.Body, originalFileLimit+1))
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if len(data) > originalFileLimit {
		return fmt.Errorf("file exceeds 50 MiB")
	}
	if err := send(tgbotapi.FileBytes{Name: path.Base(req.URL.Path), Bytes: data}); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
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
