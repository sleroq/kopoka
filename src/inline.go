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
const inlineMergedResultID = "artwork:merged"
const inlineBarsResultID = "artwork:merged:bars"

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

	photo := 0
	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		photo, _ = strconv.Atoi(link.Query().Get("photo"))
	}
	link.RawQuery = ""
	if photo > 0 {
		link.RawQuery = url.Values{"photo": {strconv.Itoa(photo)}}.Encode()
	}
	link.Fragment = ""
	return link
}

func inlineQueryLink(query string) *url.URL {
	link, _ := inlineQuerySelection(query)
	return link
}

func inlineQuerySelection(query string) (*url.URL, bool) {
	words := strings.Fields(query)
	for i, word := range words {
		link := inlineLink(word)
		if link == nil {
			continue
		}
		if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
			link.RawQuery = ""
		}
		if i+1 < len(words) && strings.Trim(words[i+1], "0123456789") == "" {
			if n, err := strconv.Atoi(words[i+1]); err == nil && n > 0 {
				if xLinkHosts[strings.ToLower(link.Host)] {
					link = xPostLink(link)
					link.Path += "/photo/" + strconv.Itoa(n)
				} else {
					link.RawQuery = url.Values{"photo": {strconv.Itoa(n)}}.Encode()
				}
				return link, true
			}
		}
		return link, false
	}
	return nil, false
}

func preferredInlineLink(db *sql.DB, user int64, query string) (*url.URL, error) {
	link, explicit := inlineQuerySelection(query)
	if link == nil || explicit {
		return link, nil
	}
	return preferredPhotoLink(db, user, link)
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

func inlineCachedMedia(kind, file string) map[string]any {
	switch kind {
	case "animation":
		return map[string]any{"type": "mpeg4_gif", "mpeg4_file_id": file}
	case "video":
		return map[string]any{"type": "video", "video_file_id": file}
	default:
		return inlinePhoto(previewPhoto{fileID: file})
	}
}

func cachedInlineResult(a artwork, previews *thumbnails) map[string]any {
	if len(a.Photos) == 1 && mediaType(a, 0) != "photo" &&
		(strings.HasSuffix(a.ID, ":merged") || strings.HasSuffix(a.ID, ":merged:bars")) {
		result := inlineCachedMedia(mediaType(a, 0), a.Photos[0])
		result["id"], result["title"], result["description"] = a.ID, a.Author, a.Text
		result["caption"], result["parse_mode"] = strings.TrimSuffix(a.Caption, "\n"), "HTML"
		return result
	}
	result := inlinePhoto(previews.savedPhoto(a))
	result["id"], result["title"], result["description"] = a.ID, a.Author, a.Text
	if len(a.Photos) == 1 && mediaType(a, 0) == "photo" && result["photo_file_id"] != nil &&
		(strings.HasSuffix(a.ID, ":merged") || strings.HasSuffix(a.ID, ":merged:bars")) {
		result["caption"], result["parse_mode"] = strings.TrimSuffix(a.Caption, "\n"), "HTML"
	} else {
		result["input_message_content"] = map[string]any{"rich_message": richArtwork(a)}
	}
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
		address := a.URL
		if link := inlineLink(a.URL); link != nil && pixivPhotoIndex(link) > 0 {
			link.RawQuery = ""
			address = link.String()
		}
		return map[string]any{"blocks": []any{map[string]any{
			"type": "animation", "animation": map[string]any{"type": "animation", "media": a.Photos[0]},
			"caption": map[string]any{"text": []any{map[string]any{"type": "url", "text": a.Author, "url": address}}},
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

func inlineHistory(db *sql.DB, query *tgbotapi.InlineQuery, source string, previews *thumbnails) ([]any, string) {
	offset, err := strconv.Atoi(query.Offset)
	if err != nil || offset < 0 {
		offset = 0
	}
	history, err := artworkHistory(db, query.From.ID, offset, source)
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

func historyFilters(query string) (source string, invert, ok bool) {
	for word := range strings.FieldsSeq(query) {
		switch {
		case word == "." && !invert:
			invert = true
		case word == "x" && source == "":
			source = "x"
		case word == "p" && source == "":
			source = "pixiv"
		default:
			return "", false, false
		}
	}
	return source, invert, true
}

func inlineHistoryView(db *sql.DB, query *tgbotapi.InlineQuery, source string, invert bool, previews *thumbnails) ([]any, string) {
	pictures, err := pictureHistoryEnabled(db, query.From.ID)
	if err != nil {
		log.Printf("loading history view: %v", err)
		return []any{}, ""
	}
	if invert {
		pictures = !pictures
	}
	if pictures {
		return inlinePictureHistory(db, query, source)
	}
	return inlineHistory(db, query, source, previews)
}

func inlinePictureHistory(db *sql.DB, query *tgbotapi.InlineQuery, source string) ([]any, string) {
	postOffset, mediaIndex := 0, 0
	if post, media, ok := strings.Cut(query.Offset, ":"); ok {
		p, pErr := strconv.Atoi(post)
		m, mErr := strconv.Atoi(media)
		if pErr == nil && mErr == nil && p >= 0 && m >= 0 {
			postOffset, mediaIndex = p, m
		}
	}

	history, err := artworkHistory(db, query.From.ID, postOffset, source)
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
	if kind == "photo" {
		result["caption"], result["parse_mode"] = strings.TrimSuffix(a.Caption, "\n"), "HTML"
	} else {
		aItem := a
		aItem.Photos, aItem.Types = []string{a.Photos[i]}, []string{kind}
		result["input_message_content"] = map[string]any{"rich_message": richArtwork(aItem)}
	}
	return result
}

func inlineArtworkResult(db *sql.DB, link *url.URL, previews *thumbnails, instant bool) map[string]any {
	a, err := findArtwork(db, artworkID(link))
	if err != nil {
		log.Printf("loading cached artwork: %v", err)
	}
	if a != nil {
		return cachedInlineResult(*a, previews)
	}

	photo, ok := previews.newPhoto(link, instant)
	if !ok {
		return nil
	}
	result := inlinePhoto(photo)
	result["id"], result["title"] = inlineResultID, "Artwork"
	result["input_message_content"] = map[string]any{
		"message_text": "Loading artwork…", "disable_web_page_preview": true,
		"entities": []any{map[string]any{"type": "text_link", "offset": 0, "length": 16, "url": link.String()}},
	}
	wait := map[string]any{"text": "Please wait…", "disabled": map[string]any{}}
	result["reply_markup"] = map[string]any{"inline_keyboard": []any{[]any{wait}}}
	return result
}

func inlineMergedResult(link *url.URL, previews *thumbnails, instant bool, bars bool) map[string]any {
	photo, ok := previews.mergedPhotoVariant(link, instant, bars)
	if !ok {
		return nil
	}
	result := inlinePhoto(photo)
	result["id"], result["title"] = inlineMergedResultID, "Merged"
	if bars {
		result["id"], result["title"] = inlineBarsResultID, "Merged with bars"
	}
	result["input_message_content"] = map[string]any{
		"message_text": "Loading merged artwork…", "disable_web_page_preview": true,
		"entities": []any{map[string]any{"type": "text_link", "offset": 0, "length": 23, "url": link.String()}},
	}
	wait := map[string]any{"text": "Please wait…", "disabled": map[string]any{}}
	result["reply_markup"] = map[string]any{"inline_keyboard": []any{[]any{wait}}}
	return result
}

func inlineArtworkResults(db *sql.DB, user int64, link *url.URL, previews *thumbnails, explicit bool) []any {
	settings, err := loadInstantInlineSettings(db, user)
	instant := settings.X

	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		instant = settings.Pixiv
	}
	if err != nil {
		instant = true
		log.Printf("loading instant inline settings: %v", err)
	}
	if explicit {
		if result := inlineArtworkResult(db, link, previews, instant); result != nil {
			return []any{result}
		}
		return nil
	}
	normal := make(chan map[string]any, 1)
	merged := make(chan map[string]any, 1)
	bars := make(chan map[string]any, 1)
	go func() { normal <- inlineArtworkResult(db, link, previews, instant) }()
	go func() { merged <- inlineMergedResult(link, previews, instant, false) }()
	go func() { bars <- inlineMergedResult(link, previews, instant, true) }()

	var postImages []any
	if xLinkHosts[strings.ToLower(link.Host)] {
		postImages = inlinePostImageResults(db, user, link, previews, instant)
	}

	var results []any
	if result := <-normal; result != nil {
		results = append(results, result)
	}
	if result := <-merged; result != nil {
		results = append(results, result)
	}
	if result := <-bars; result != nil {
		results = append(results, result)
	}
	results = append(results, postImages...)
	if xLinkHosts[strings.ToLower(link.Host)] {
		results = append(results, inlinePostTextResult(link, previews))
	}
	return results
}

func inlineQueryResults(db *sql.DB, query *tgbotapi.InlineQuery, previews *thumbnails) ([]any, string) {
	if family, filter, ok := themeQuery(query.Query); ok {
		return themeResults(db, query.From.ID, family, filter, previews), ""
	}
	if _, ok := timezoneQuery(query.Query); ok {
		return timezoneResults(query)
	}
	if source, invert, ok := historyFilters(query.Query); ok {
		return inlineHistoryView(db, query, source, invert, previews)
	}
	if link := inlineQueryLink(query.Query); link != nil {
		link, err := preferredInlineLink(db, query.From.ID, query.Query)
		if err != nil {
			log.Printf("loading photo preference: %v", err)
			return []any{}, ""
		}
		_, explicit := inlineQuerySelection(query.Query)
		return inlineArtworkResults(db, query.From.ID, link, previews, explicit), ""
	}
	return []any{}, ""
}

func handleInlineQuery(bot *tgbotapi.BotAPI, db *sql.DB, query *tgbotapi.InlineQuery, previews *thumbnails) {
	results, nextOffset := inlineQueryResults(db, query, previews)
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
	if strings.HasPrefix(chosen.ResultID, "theme:") {
		handleThemeSelection(bot, db, chosen)
		return
	}
	if strings.HasPrefix(chosen.ResultID, "timezone:") {
		handleTimezoneSelection(bot, db, chosen)
		return
	}
	link := inlineQueryLink(chosen.Query)
	theme := ""
	if link != nil && link.Host != "pixiv.net" && link.Host != "www.pixiv.net" {
		theme = postImageTheme(chosen.ResultID, link)
	}
	if theme == "" && chosen.ResultID != inlinePostTextID && chosen.ResultID != inlineResultID && chosen.ResultID != inlineMergedResultID && chosen.ResultID != inlineBarsResultID {
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
	if theme != "" && strings.HasSuffix(chosen.ResultID, ":cached") {
		if err := selectArtwork(db, chosen.From.ID, artworkID(xPostLink(link))); err != nil {
			log.Printf("recording inline selection: %v", err)
		}
		return
	}
	if chosen.InlineMessageID == "" {
		log.Print("missing inline message ID")
		return
	}
	result, err := queue.submit(chosen.From.ID, func() error {
		if theme != "" {
			return editInlinePostImage(bot, db, chosen, cacheChat, link, theme, previews)
		}
		if chosen.ResultID == inlinePostTextID {
			if link == nil || !xLinkHosts[strings.ToLower(link.Host)] {
				return fmt.Errorf("invalid X post URL")
			}
			return editInlinePostText(bot, db, chosen, cacheChat, link)
		}
		return editInlineArtwork(bot, app, db, chosen, cacheChat, previews)
	})
	label := "artwork"
	if chosen.ResultID == inlinePostTextID {
		label = "post"
	}
	failure := ""
	if err != nil {
		failure = fmt.Sprintf("Could not load %s: %s. Please retry later.", label, err)
	} else if err := <-result; err != nil {
		log.Printf("processing chosen inline result: %v", err)
		failure = fmt.Sprintf("Could not load %s. Please try again.", label)
		if _, ok := errors.AsType[unavailablePhotoError](err); ok || errors.Is(err, errXPostNotFound) {
			failure = fmt.Sprintf("Could not load %s: %s.", label, err)
		}
	}
	if failure != "" {
		for word := range strings.FieldsSeq(chosen.Query) {
			if inlineLink(word) != nil {
				failure += "\n\n" + word
				break
			}
		}
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
	link := inlineQueryLink(chosen.Query)
	if link == nil {
		return fmt.Errorf("invalid artwork URL")
	}

	link, err := preferredInlineLink(db, chosen.From.ID, chosen.Query)
	if err != nil {
		return err
	}
	a := artwork{ID: artworkID(link), URL: link.String()}
	merge := ""
	switch chosen.ResultID {
	case inlineMergedResultID:
		merge = "1"
		a.ID += ":merged"
	case inlineBarsResultID:
		merge = "bars"
		a.ID += ":merged:bars"
	}
	photos, err := fetchArtPhotos(app, link, &a, merge)
	if err != nil {
		return err
	}
	if len(photos) == 0 {
		if xLinkHosts[strings.ToLower(link.Host)] {
			themes, err := loadPostImageThemes(db, chosen.From.ID)
			if err != nil {
				return err
			}
			return editInlinePostImage(bot, db, chosen, cacheChat, link, themes.Dark, previews)
		}
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
	} else if previews != nil && merge == "" {
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
