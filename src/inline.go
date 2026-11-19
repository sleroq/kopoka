package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const inlineResultID = "artwork"
const inlineMergedResultID = "artwork:merged"
const inlineBarsResultID = "artwork:merged:bars"
const inlineFeedbackID = "feedback:"

func inlineFeedback(id, title, description, message string) map[string]any {
	return map[string]any{
		"type": "article", "id": inlineFeedbackID + id, "title": title, "description": description,
		"input_message_content": map[string]any{"message_text": message, "disable_web_page_preview": true},
	}
}

func inlinePreferenceFailure() []any {
	message := "Could not load preferences. Please try again."
	return []any{inlineFeedback("preferences", "Could not load preferences", "Please try again.", message)}
}

func inlineLink(text string) *url.URL {
	link, err := url.Parse(strings.TrimSpace(text))
	if err != nil || (link.Scheme != "https" && link.Scheme != "http") || !supportedLink(link) || link.User != nil {
		return nil
	}
	if path, ok := strings.CutSuffix(link.Path, "/"); ok {
		link.Path = path
		link.RawPath = ""
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
		if kind, index := xMediaSelection(link); index > 0 {
			parts = parts[:3]
			link.Path = "/" + strings.Join(parts, "/") + "/" + kind + "/" + strconv.Itoa(index)
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
					link.Path += "/media/" + strconv.Itoa(n)
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

func cachedInlineResult(a artwork, previews *thumbnails) map[string]any {
	result := inlinePhoto(previews.savedPhoto(a))
	result["id"], result["title"], result["description"] = a.ID, a.Author, a.Text
	if len(a.Photos) == 1 && (strings.HasSuffix(a.ID, ":merged") || strings.HasSuffix(a.ID, ":merged:bars")) {
		result["id"] = a.ID + ":preview"
		result["input_message_content"] = map[string]any{
			"message_text": "Loading merged artwork…", "disable_web_page_preview": true,
		}
		wait := map[string]any{"text": "Please wait…", "disabled": map[string]any{}}
		result["reply_markup"] = map[string]any{"inline_keyboard": []any{[]any{wait}}}
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
		return inlinePreferenceFailure(), ""
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
	results := make(chan []any, 1)
	go func() { results <- previewArtworkResults(db, user, link, previews, explicit) }()
	timer := time.NewTimer(inlinePreviewBudget)
	defer timer.Stop()
	select {
	case ready := <-results:
		return ready
	case <-timer.C:
		return inlineArtworkFallback(db, link, previews)
	}
}

func inlineArtworkFallback(db *sql.DB, link *url.URL, previews *thumbnails) []any {
	return []any{inlineArtworkResult(db, link, previews, true)}
}

func previewArtworkResults(db *sql.DB, user int64, link *url.URL, previews *thumbnails, explicit bool) []any {
	settings, err := loadInstantInlineSettings(db, user)
	instant := settings.X

	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		instant = settings.Pixiv
	}
	if err != nil {
		log.Printf("loading instant inline settings: %v", err)
		return inlinePreferenceFailure()
	}
	if !instant {
		if err := inlinePreviewSource(db, link, previews); err != nil {
			if errors.Is(err, errXPostNotFound) {
				title, detail := "Post not found (404)", "Check the link and try again."
				return []any{inlineFeedback("source", title, detail, title+". "+detail+"\n\n"+link.String())}
			}
			return inlineArtworkFallback(db, link, previews)
		}
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
	var postText chan map[string]any
	if xLinkHosts[strings.ToLower(link.Host)] {
		postText = make(chan map[string]any, 1)
		go func() { postText <- inlinePostTextResult(link, previews, instant) }()
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
	if postText != nil {
		results = append(results, <-postText)
	}
	return results
}

func inlinePreviewSource(db *sql.DB, link *url.URL, previews *thumbnails) error {
	cached, err := findArtwork(db, artworkID(link))
	if err != nil {
		log.Printf("loading cached artwork: %v", err)
	}
	if cached != nil {
		return nil
	}
	work := previews.warmup(link, 0)
	select {
	case <-work.sourceDone:
		if errors.Is(work.sourceErr, errNoXMedia) {
			return nil
		}
		return work.sourceErr
	case <-time.After(inlinePreviewBudget):
		return context.DeadlineExceeded
	}
}

func inlineQueryResults(db *sql.DB, query *tgbotapi.InlineQuery, previews *thumbnails) ([]any, string) {
	if family, filter, ok := themeQuery(query.Query); ok {
		return themeResults(db, query.From.ID, family, filter, previews), ""
	}
	if mode, filter, ok := translationQuery(query.Query); ok {
		return translationResults(db, query, mode, filter)
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
			return inlinePreferenceFailure(), ""
		}
		_, explicit := inlineQuerySelection(query.Query)
		return inlineArtworkResults(db, query.From.ID, link, previews, explicit), ""
	}
	guide := "Search guide\n\n" +
		"• Paste an X or Pixiv post link to choose a view.\n" +
		"• Add a number after the link to select a media item or page.\n" +
		"• Leave the query empty for history; use x or p to filter, and . to switch history view.\n" +
		"• Use theme light or theme dark to choose a post image theme.\n" +
		"• Use language target or language ignore to search languages.\n" +
		"• Use tz to search time zones. Use /settings for more options."
	return []any{inlineFeedback("guide", "Query not understood", "Select for a usage guide.", guide)}, ""
}

func handleInlineQuery(bot *tgbotapi.BotAPI, db *sql.DB, query *tgbotapi.InlineQuery, previews *thumbnails) {
	until := time.Now().Add(inlineAnswerBudget)
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
	ctx, cancel := context.WithDeadline(context.Background(), until)
	defer cancel()
	bot = telegramWithContext(bot, ctx)
	if _, err := retryInline(&inlineRetry{until: until}, func() (*tgbotapi.APIResponse, error) {
		return bot.MakeRequest("answerInlineQuery", params)
	}); err != nil {
		log.Printf("answering inline query: %v", err)
	}
}

func handleChosenInlineResult(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, cacheChat string, queue *mediaQueue, previews *thumbnails) {
	if strings.HasPrefix(chosen.ResultID, inlineFeedbackID) {
		return
	}
	if strings.HasPrefix(chosen.ResultID, "theme:") {
		handleThemeSelection(bot, db, chosen)
		return
	}
	if strings.HasPrefix(chosen.ResultID, "language:") {
		handleTranslationSelection(bot, db, chosen)
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
	cachedID, cachedPreview := strings.CutSuffix(chosen.ResultID, ":preview")
	if !cachedPreview && theme == "" && chosen.ResultID != inlinePostTextID && chosen.ResultID != inlineResultID && chosen.ResultID != inlineMergedResultID && chosen.ResultID != inlineBarsResultID {
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
		retry := newInlineDelivery(bot, chosen.InlineMessageID)
		ctx, cancel := context.WithDeadline(context.Background(), retry.until)
		defer cancel()
		bot := telegramWithContext(bot, ctx)
		switch {
		case cachedPreview:
			return editCachedInlineArtwork(bot, db, chosen, cachedID, retry)
		case theme != "":
			return editInlinePostImage(bot, db, chosen, cacheChat, link, theme, previews, retry)
		case chosen.ResultID == inlinePostTextID:
			if link == nil || !xLinkHosts[strings.ToLower(link.Host)] {
				return fmt.Errorf("invalid X post URL")
			}
			return editInlinePostText(bot, db, chosen, cacheChat, link, retry)
		default:
			return editInlineArtwork(bot, app, db, chosen, cacheChat, previews, retry)
		}
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		until, _ := ctx.Deadline()
		if editErr := editInlineMessage(telegramWithContext(bot, ctx), &inlineRetry{until: until}, "editMessageText", params); editErr != nil {
			log.Printf("editing inline failure message: %v", editErr)
		}
	}
}

func editCachedInlineArtwork(bot *tgbotapi.BotAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, id string, retry *inlineRetry) error {
	a, err := findArtwork(db, id)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("cached artwork not found")
	}
	payload, err := json.Marshal(map[string]any{
		"type": mediaType(*a, 0), "media": a.Photos[0], "caption": a.Caption, "parse_mode": "HTML",
	})
	if err != nil {
		return err
	}
	params := tgbotapi.Params{
		"inline_message_id": chosen.InlineMessageID, "media": string(payload),
		"reply_markup": `{"inline_keyboard":[]}`,
	}
	if err := editInlineMessage(bot, retry, "editMessageMedia", params); err != nil {
		return err
	}
	if err := selectArtwork(db, chosen.From.ID, id); err != nil {
		log.Printf("recording inline selection: %v", err)
	}
	return nil
}

func editInlineArtwork(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, cacheChat string, previews *thumbnails, retry *inlineRetry) error {
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
	downloading := inlineMediaProgress(bot, chosen.InlineMessageID, "Downloading")
	photos, err := retryInline(retry, func() ([]any, error) {
		address := *link
		photos, err := fetchArtPhotos(app, &address, &a, merge, downloading)
		if err == nil {
			link = &address
		}
		return photos, err
	})
	if err != nil {
		return err
	}
	if len(photos) == 0 {
		if xLinkHosts[strings.ToLower(link.Host)] {
			themes, err := loadPostImageThemes(db, chosen.From.ID)
			if err != nil {
				return err
			}
			return editInlinePostImage(bot, db, chosen, cacheChat, link, themes.Dark, previews, retry)
		}
		return fmt.Errorf("artwork has no photos")
	}
	if len(photos) > 50 {
		return fmt.Errorf("inline slideshow exceeds 50 photos: %d", len(photos))
	}

	uploading := inlineMediaProgress(bot, chosen.InlineMessageID, "Uploading")
	if err := cacheInlineArtworkMedia(bot, cacheChat, photos, &a, uploading, retry); err != nil {
		return err
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
	if err := editInlineMessage(bot, retry, method, params); err != nil {
		return fmt.Errorf("editing inline artwork: %w", err)
	}

	if err := saveSelection(db, chosen.From.ID, a); err != nil {
		log.Printf("saving inline artwork: %v", err)
	} else if previews != nil && merge == "" {
		previews.warmArtwork(inlineLink(a.URL), len(a.Photos))
	}
	return nil
}

func inlineMediaProgress(bot *tgbotapi.BotAPI, messageID, phase string) func(done, total int) {
	var last time.Time
	return func(done, total int) {
		if total <= 5 || done != 0 && done != total && time.Since(last) < time.Second {
			return
		}
		last = time.Now()
		filled := done * 8 / total
		label := fmt.Sprintf("%s %s%s %d/%d", phase, strings.Repeat("█", filled), strings.Repeat("░", 8-filled), done, total)
		if err := editInlineButton(bot, messageID, label); err != nil {
			log.Printf("updating inline media progress: %v", err)
		}
	}
}

func cacheInlineArtworkMedia(bot *tgbotapi.BotAPI, chat string, media []any, a *artwork, progress func(done, total int), retry *inlineRetry) error {
	progress(0, len(media))
	for start := 0; start < len(media); {
		end := start + 1
		if mediaBase(media[start]).Type == "photo" {
			for end < min(start+10, len(media)) && mediaBase(media[end]).Type == "photo" {
				end++
			}
		}
		if end-start == 1 {
			kind, id, err := cacheInlineMediaWithVideoRetry(bot, chat, media[start], true, retry)
			if err != nil {
				return err
			}
			a.Photos = append(a.Photos, id)
			a.Types = append(a.Types, kind)
		} else {
			ids, err := cacheInlinePhotoAlbum(bot, chat, media[start:end], retry)
			if err != nil {
				return err
			}
			a.Photos = append(a.Photos, ids...)
			for range ids {
				a.Types = append(a.Types, "photo")
			}
		}
		progress(end, len(media))
		start = end
	}
	return nil
}

func cacheInlinePhotoAlbum(bot *tgbotapi.BotAPI, chat string, photos []any, retry *inlineRetry) ([]string, error) {
	chatID, channel, err := inlineCacheTarget(chat)
	if err != nil {
		return nil, err
	}
	group := tgbotapi.NewMediaGroup(chatID, photos)
	group.ChannelUsername = channel
	messages, err := retryInlineTelegram(retry, func() ([]tgbotapi.Message, error) { return bot.SendMediaGroup(group) })
	if err != nil {
		return nil, fmt.Errorf("uploading inline photo album: %w", err)
	}

	messageIDs := make([]int, 0, len(messages))
	for _, message := range messages {
		messageIDs = append(messageIDs, message.MessageID)
	}
	if len(messageIDs) > 0 {
		encoded, _ := json.Marshal(messageIDs)
		params := tgbotapi.Params{"chat_id": chat, "message_ids": string(encoded)}
		if _, err := bot.MakeRequest("deleteMessages", params); err != nil {
			log.Printf("deleting inline cache album: %v", err)
		}
	}
	if len(messages) != len(photos) {
		return nil, fmt.Errorf("inline upload returned %d of %d photos", len(messages), len(photos))
	}
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		id, err := inlinePhotoID(message)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func inlineCacheTarget(chat string) (int64, string, error) {
	if strings.HasPrefix(chat, "@") {
		return 0, chat, nil
	}
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil || id == 0 {
		return 0, "", fmt.Errorf("invalid INLINE_CACHE_CHAT_ID: %q", chat)
	}
	return id, "", nil
}

func cacheInlineMedia(bot *tgbotapi.BotAPI, chat string, item any) (string, string, error) {
	return cacheInlineMediaWithVideoRetry(bot, chat, item, true, nil)
}

func cacheInlineMediaWithVideoRetry(bot *tgbotapi.BotAPI, chat string, item any, retryVideo bool, retry *inlineRetry) (string, string, error) {
	chatID, channel, err := inlineCacheTarget(chat)
	if err != nil {
		return "", "", err
	}

	base := mediaBase(item)
	send := func() (tgbotapi.Message, error) {
		if base.Type == "animation" {
			config := tgbotapi.NewAnimation(chatID, base.Media)
			config.ChannelUsername = channel
			return bot.Send(config)
		}
		if base.Type == "video" {
			video := item.(tgbotapi.InputMediaVideo)
			video.BaseInputMedia = base
			config := streamingVideo(chatID, video)
			config.ChannelUsername = channel
			return bot.Send(config)
		}
		config := tgbotapi.NewPhoto(chatID, base.Media)
		config.ChannelUsername = channel
		return bot.Send(config)
	}
	message, err := retryInlineTelegram(retry, send)
	if address, ok := base.Media.(tgbotapi.FileURL); retryVideo && ok && base.Type == "video" && telegramURLFailure(err) {
		file, downloadErr := retryInline(retry, func() (tgbotapi.FilePath, error) { return downloadVideo(string(address)) })
		if downloadErr != nil {
			return "", "", fmt.Errorf("uploading inline media: %w", downloadErr)
		}
		defer func() { _ = os.Remove(string(file)) }()
		base.Media = file
		message, err = retryInlineTelegram(retry, send)
	}
	if err != nil {
		return "", "", fmt.Errorf("uploading inline media: %w", err)
	}

	deletion := tgbotapi.DeleteMessageConfig{ChatID: chatID, ChannelUsername: channel, MessageID: message.MessageID}
	if _, err := bot.Request(deletion); err != nil {
		log.Printf("deleting inline cache message: %v", err)
	}

	if base.Type == "animation" {
		if message.Animation == nil || message.Animation.FileID == "" {
			return "", "", fmt.Errorf("inline upload returned no animation")
		}
		return "animation", message.Animation.FileID, nil
	}
	if base.Type == "video" {
		if retryVideo && message.Animation != nil && message.Animation.FileID != "" {
			return "animation", message.Animation.FileID, nil
		}
		if message.Video == nil || message.Video.FileID == "" {
			return "", "", fmt.Errorf("inline upload returned no video")
		}
		return "video", message.Video.FileID, nil
	}
	id, err := inlinePhotoID(message)
	return "photo", id, err
}

func inlinePhotoID(message tgbotapi.Message) (string, error) {
	if len(message.Photo) == 0 || message.Photo[len(message.Photo)-1].FileID == "" {
		return "", fmt.Errorf("inline upload returned no photo")
	}
	return message.Photo[len(message.Photo)-1].FileID, nil
}
