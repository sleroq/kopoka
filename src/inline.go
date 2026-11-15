package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const inlineResultID = "artwork"

func inlineLink(text string) *url.URL {
	link, err := url.Parse(strings.TrimSpace(text))
	if err != nil || (link.Scheme != "https" && link.Scheme != "http") || !supportedLink(link) || link.User != nil {
		return nil
	}
	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		if strings.HasPrefix(link.Path, "/en/artworks/") {
			link.Path = strings.TrimPrefix(link.Path, "/en")
			link.RawPath = ""
		}
		id, ok := strings.CutPrefix(link.Path, "/artworks/")
		n, err := strconv.ParseUint(id, 10, 64)
		if !ok || err != nil || n == 0 {
			return nil
		}
	} else {
		parts := strings.Split(strings.TrimPrefix(link.Path, "/"), "/")
		if index := xPhotoIndex(link); index > 0 {
			parts = parts[:3]
			link.Path = "/" + strings.Join(parts, "/") + "/photo/" + strconv.Itoa(index)
			link.RawPath = ""
		}
		if len(parts) != 3 || parts[0] == "" || parts[1] != "status" {
			return nil
		}
		id, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil || id == 0 {
			return nil
		}
	}
	link.RawQuery = ""
	link.Fragment = ""
	return link
}

func inlinePhoto(photo previewPhoto) map[string]any {
	result := map[string]any{"type": "photo"}
	if photo.fileID != "" {
		result["photo_file_id"] = photo.fileID
	} else {
		result["photo_url"], result["thumbnail_url"] = photo.photoURL, photo.thumbnailURL
		result["photo_width"], result["photo_height"] = photo.width, photo.height
	}
	return result
}

func cachedInlineResult(a artwork, previews *thumbnails) map[string]any {
	result := inlinePhoto(previews.savedPhoto(a))
	result["id"], result["title"], result["description"] = a.ID, a.Author, a.Text
	result["input_message_content"] = map[string]any{"rich_message": richArtwork(a)}
	return result
}

func mediaType(a artwork, i int) string {
	if len(a.Types) == 0 {
		return "photo"
	}
	return a.Types[i]
}

func richArtwork(a artwork) map[string]any {
	if len(a.Photos) > 1 {
		return slideshow(a)
	}
	kind := mediaType(a, 0)
	if kind == "animation" {
		return map[string]any{"blocks": []any{map[string]any{
			"type": "animation", "animation": map[string]any{"type": "animation", "media": a.Photos[0]},
			"caption": map[string]any{"text": []any{map[string]any{"type": "url", "text": a.Author, "url": a.URL}}},
		}}}
	}
	media := []any{map[string]any{"id": "0", "media": map[string]any{"type": kind, "media": a.Photos[0]}}}
	markup := `<img src="tg://photo?id=0"/>`
	if kind == "video" {
		markup = `<video src="tg://video?id=0"></video>`
	}
	html := fmt.Sprintf("<figure>%s<figcaption>%s</figcaption></figure>", markup, strings.TrimSuffix(a.Caption, "\n"))
	return map[string]any{"html": html, "media": media}
}

func slideshow(a artwork) map[string]any {
	var markup strings.Builder
	markup.WriteString("<tg-slideshow>")
	media := make([]any, 0, len(a.Photos))
	for i, id := range a.Photos {
		key := strconv.Itoa(i)
		kind := mediaType(a, i)
		if kind == "video" {
			fmt.Fprintf(&markup, `<video src="tg://video?id=%s"></video>`, key)
		} else {
			fmt.Fprintf(&markup, `<img src="tg://photo?id=%s"/>`, key)
		}
		media = append(media, map[string]any{"id": key, "media": map[string]any{"type": kind, "media": id}})
	}
	fmt.Fprintf(&markup, "<figcaption>%s</figcaption></tg-slideshow>", strings.TrimSuffix(a.Caption, "\n"))
	return map[string]any{"html": markup.String(), "media": media}
}

func inlineHistory(db *sql.DB, query *tgbotapi.InlineQuery, previews *thumbnails) ([]any, string) {
	offset, err := strconv.Atoi(query.Offset)
	if err != nil || offset < 0 {
		offset = 0
	}
	history, err := artworkHistory(db, query.From.ID, offset)
	if err != nil {
		log.Printf("loading inline history: %v", err)
	}
	nextOffset := ""
	if len(history) > 50 {
		history = history[:50]
		nextOffset = strconv.Itoa(offset + 50)
	}
	results := []any{}
	for _, a := range history {
		results = append(results, cachedInlineResult(a, previews))
	}
	return results, nextOffset
}

func inlineHistoryView(db *sql.DB, query *tgbotapi.InlineQuery, previews *thumbnails) ([]any, string) {
	pictures, err := pictureHistoryEnabled(db, query.From.ID)
	if err != nil {
		log.Printf("loading history view: %v", err)
		return []any{}, ""
	}
	if strings.TrimSpace(query.Query) == "." {
		pictures = !pictures
	}
	if pictures {
		return inlinePictureHistory(db, query)
	}
	return inlineHistory(db, query, previews)
}

func inlinePictureHistory(db *sql.DB, query *tgbotapi.InlineQuery) ([]any, string) {
	postOffset, mediaIndex := 0, 0
	if post, media, ok := strings.Cut(query.Offset, ":"); ok {
		p, pErr := strconv.Atoi(post)
		m, mErr := strconv.Atoi(media)
		if pErr == nil && mErr == nil && p >= 0 && m >= 0 {
			postOffset, mediaIndex = p, m
		}
	}

	history, err := artworkHistory(db, query.From.ID, postOffset)
	if err != nil {
		log.Printf("loading inline history: %v", err)
	}

	results := []any{}
	for post, a := range history {
		start := 0
		if post == 0 {
			start = mediaIndex
		}
		for i := start; i < len(a.Photos); i++ {
			if len(results) == 50 {
				return results, fmt.Sprintf("%d:%d", postOffset+post, i)
			}
			result := cachedPictureResult(a, i)
			results = append(results, result)
		}
	}
	return results, ""
}

func cachedPictureResult(a artwork, i int) map[string]any {
	kind := mediaType(a, i)
	result := map[string]any{"id": a.ID + ":item:" + strconv.Itoa(i), "title": a.Author, "description": a.Text}
	switch kind {
	case "video":
		result["type"], result["video_file_id"] = "video", a.Photos[i]
	case "animation":
		result["type"], result["mpeg4_file_id"] = "mpeg4_gif", a.Photos[i]
	default:
		result["type"], result["photo_file_id"] = "photo", a.Photos[i]
	}
	aItem := a
	aItem.Photos, aItem.Types = []string{a.Photos[i]}, []string{kind}
	result["input_message_content"] = map[string]any{"rich_message": richArtwork(aItem)}
	return result
}

func inlineArtworkResult(db *sql.DB, user int64, link *url.URL, previews *thumbnails) map[string]any {
	a, err := findArtwork(db, artworkID(link))
	if err != nil {
		log.Printf("loading cached artwork: %v", err)
	}
	if a != nil {
		return cachedInlineResult(*a, previews)
	}

	settings, err := loadInstantInlineSettings(db, user)
	pixiv := link.Host == "pixiv.net" || link.Host == "www.pixiv.net"
	instant := settings.X
	if pixiv {
		instant = settings.Pixiv
	}
	if err != nil {
		log.Printf("loading instant inline settings: %v", err)
	}
	result := inlinePhoto(previews.newPhoto(link, err != nil || instant))
	result["id"], result["title"] = inlineResultID, "Artwork"
	result["input_message_content"] = map[string]any{
		"message_text": "Loading artwork…", "disable_web_page_preview": true,
		"entities": []any{map[string]any{"type": "text_link", "offset": 0, "length": 16, "url": link.String()}},
	}
	wait := map[string]any{"text": "Please wait…", "disabled": map[string]any{}}
	result["reply_markup"] = map[string]any{"inline_keyboard": []any{[]any{wait}}}
	return result
}

func handleInlineQuery(bot *tgbotapi.BotAPI, db *sql.DB, query *tgbotapi.InlineQuery, previews *thumbnails) {
	results := []any{}
	nextOffset := ""

	if text := strings.TrimSpace(query.Query); text == "" || text == "." {
		results, nextOffset = inlineHistoryView(db, query, previews)
	} else if link := inlineLink(query.Query); link != nil {
		link, err := preferredPhotoLink(db, query.From.ID, link)
		if err != nil {
			log.Printf("loading photo preference: %v", err)
		} else {
			results = append(results, inlineArtworkResult(db, query.From.ID, link, previews))
		}
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		log.Printf("encoding inline results: %v", err)
		return
	}
	params := tgbotapi.Params{
		"inline_query_id": query.ID, "results": string(encoded), "cache_time": "0",
		"is_personal": "true", "next_offset": nextOffset,
	}
	if _, err := bot.MakeRequest("answerInlineQuery", params); err != nil {
		log.Printf("answering inline query: %v", err)
	}
}

func handleChosenInlineResult(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, cacheChat string, queue *mediaQueue, previews *thumbnails) {
	if chosen.ResultID != inlineResultID {
		id := chosen.ResultID
		if parent, index, ok := strings.Cut(id, ":item:"); ok {
			if n, err := strconv.Atoi(index); err == nil && n >= 0 {
				id = parent
			}
		}

		if err := selectArtwork(db, chosen.From.ID, id); err != nil {
			log.Printf("recording inline selection: %v", err)
		}
		return
	}
	if chosen.InlineMessageID == "" {
		log.Print("missing inline message ID")
		return
	}
	result, err := queue.submit(chosen.From.ID, func() error {
		return editInlineArtwork(bot, app, db, chosen, cacheChat, previews)
	})
	failure := ""
	if err != nil {
		failure = fmt.Sprintf("Could not load artwork: %s. Please retry later.", err)
	} else if err := <-result; err != nil {
		log.Printf("processing chosen inline result: %v", err)
		failure = "Could not load artwork. Please try again."
		if _, ok := errors.AsType[unavailablePhotoError](err); ok {
			failure = "Could not load artwork: " + err.Error() + "."
		}
	}
	if failure != "" {
		params := tgbotapi.Params{
			"inline_message_id": chosen.InlineMessageID, "text": failure,
			"reply_markup": `{"inline_keyboard":[]}`,
		}
		if _, editErr := retryTelegram(func() (*tgbotapi.APIResponse, error) {
			return bot.MakeRequest("editMessageText", params)
		}); editErr != nil {
			log.Printf("editing inline failure message: %v", editErr)
		}
	}
}

func editInlineArtwork(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, cacheChat string, previews *thumbnails) error {
	link := inlineLink(chosen.Query)
	if link == nil {
		return fmt.Errorf("invalid artwork URL")
	}

	link, err := preferredPhotoLink(db, chosen.From.ID, link)
	if err != nil {
		return err
	}
	a := artwork{ID: artworkID(link), URL: link.String()}
	photos, err := fetchArtPhotos(app, link, &a)
	if err != nil {
		return err
	}
	if len(photos) == 0 {
		return fmt.Errorf("artwork has no photos")
	}
	if len(photos) > 50 {
		return fmt.Errorf("inline slideshow exceeds 50 photos: %d", len(photos))
	}

	for _, item := range photos {
		id, err := cacheInlineMedia(bot, cacheChat, item)
		if err != nil {
			return err
		}
		a.Photos = append(a.Photos, id)
		a.Types = append(a.Types, mediaBase(item).Type)
	}

	first := mediaBase(photos[0])
	a.Caption = first.Caption
	params := tgbotapi.Params{"inline_message_id": chosen.InlineMessageID, "reply_markup": `{"inline_keyboard":[]}`}
	method := "editMessageMedia"
	var payload any = map[string]any{"type": a.Types[0], "media": a.Photos[0], "caption": a.Caption, "parse_mode": first.ParseMode}
	if len(a.Photos) > 1 {
		method = "editMessageText"
		payload = slideshow(a)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding inline artwork: %w", err)
	}
	if len(a.Photos) == 1 {
		params["media"] = string(encoded)
	} else {
		params["rich_message"] = string(encoded)
	}
	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) { return bot.MakeRequest(method, params) }); err != nil {
		return fmt.Errorf("editing inline artwork: %w", err)
	}

	if err := saveSelection(db, chosen.From.ID, a); err != nil {
		log.Printf("saving inline artwork: %v", err)
	} else if previews != nil {
		previews.warmArtwork(inlineLink(a.URL), len(a.Photos))
	}
	return nil
}

func cacheInlineMedia(bot *tgbotapi.BotAPI, chat string, item any) (string, error) {
	var chatID int64
	var channel string
	if strings.HasPrefix(chat, "@") {
		channel = chat
	} else {
		id, err := strconv.ParseInt(chat, 10, 64)
		if err != nil || id == 0 {
			return "", fmt.Errorf("invalid INLINE_CACHE_CHAT_ID: %q", chat)
		}
		chatID = id
	}

	base := mediaBase(item)
	message, err := retryTelegram(func() (tgbotapi.Message, error) {
		if base.Type == "animation" {
			config := tgbotapi.NewAnimation(chatID, base.Media)
			config.ChannelUsername = channel
			return bot.Send(config)
		}
		if base.Type == "video" {
			config := tgbotapi.NewVideo(chatID, base.Media)
			config.ChannelUsername = channel
			return bot.Send(config)
		}
		config := tgbotapi.NewPhoto(chatID, base.Media)
		config.ChannelUsername = channel
		return bot.Send(config)
	})
	if err != nil {
		return "", fmt.Errorf("uploading inline media: %w", err)
	}

	deletion := tgbotapi.DeleteMessageConfig{ChatID: chatID, ChannelUsername: channel, MessageID: message.MessageID}
	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) { return bot.Request(deletion) }); err != nil {
		log.Printf("deleting inline cache message: %v", err)
	}

	if base.Type == "animation" {
		if message.Animation == nil || message.Animation.FileID == "" {
			return "", fmt.Errorf("inline upload returned no animation")
		}
		return message.Animation.FileID, nil
	}
	if base.Type == "video" {
		if message.Video == nil || message.Video.FileID == "" {
			return "", fmt.Errorf("inline upload returned no video")
		}
		return message.Video.FileID, nil
	}
	if len(message.Photo) == 0 {
		return "", fmt.Errorf("inline upload returned no photo")
	}
	return message.Photo[len(message.Photo)-1].FileID, nil
}
