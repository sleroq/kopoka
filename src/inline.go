package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func handleInlineQuery(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, query *tgbotapi.InlineQuery, cacheChat string) {
	results, err := inlineResults(bot, app, query.Query, cacheChat)
	if err != nil {
		log.Printf("processing inline query: %v", err)
	}
	if results == nil {
		results = []any{}
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		log.Printf("encoding inline results: %v", err)
		encoded = []byte("[]")
	}
	params := tgbotapi.Params{
		"inline_query_id": query.ID, "results": string(encoded), "cache_time": "0",
	}
	if _, err := bot.MakeRequest("answerInlineQuery", params); err != nil {
		log.Printf("answering inline query: %v", err)
	}
}

func inlineResults(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, text, cacheChat string) ([]any, error) {
	link, err := url.Parse(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("parsing inline URL: %w", err)
	}
	if (link.Scheme != "https" && link.Scheme != "http") || !supportedLink(link) {
		return nil, nil
	}
	link.RawQuery = ""
	photos, err := fetchArtPhotos(app, link)
	if err != nil {
		return nil, err
	}
	if len(photos) == 0 {
		return nil, nil
	}
	if len(photos) > 50 {
		return nil, fmt.Errorf("inline slideshow exceeds 50 photos: %d", len(photos))
	}
	ids := make([]string, 0, len(photos))
	for _, item := range photos {
		id, err := cacheInlinePhoto(bot, cacheChat, item.(tgbotapi.InputMediaPhoto))
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return []any{inlinePhotoResult(ids, photos[0].(tgbotapi.InputMediaPhoto))}, nil
}

func inlinePhotoResult(ids []string, first tgbotapi.InputMediaPhoto) any {
	if len(ids) == 1 {
		return map[string]any{
			"type": "photo", "id": "photo", "photo_file_id": ids[0],
			"caption": first.Caption, "parse_mode": first.ParseMode,
		}
	}
	var markup strings.Builder
	markup.WriteString("<tg-slideshow>")
	media := make([]any, 0, len(ids))
	for i, id := range ids {
		key := strconv.Itoa(i)
		fmt.Fprintf(&markup, `<img src="tg://photo?id=%s"/>`, key)
		media = append(media, map[string]any{"id": key, "media": map[string]any{"type": "photo", "media": id}})
	}
	fmt.Fprintf(&markup, "<figcaption>%s</figcaption></tg-slideshow>", first.Caption)
	rich := map[string]any{"html": markup.String(), "media": media}
	return map[string]any{
		"type": "article", "id": "slideshow", "title": "Artwork",
		"input_message_content": map[string]any{"rich_message": rich},
	}
}

func cacheInlinePhoto(bot *tgbotapi.BotAPI, chat string, photo tgbotapi.InputMediaPhoto) (string, error) {
	config := tgbotapi.NewPhoto(0, photo.Media)
	if strings.HasPrefix(chat, "@") {
		config.ChannelUsername = chat
	} else {
		id, err := strconv.ParseInt(chat, 10, 64)
		if err != nil || id == 0 {
			return "", fmt.Errorf("invalid INLINE_CACHE_CHAT_ID: %q", chat)
		}
		config.ChatID = id
	}
	message, err := bot.Send(config)
	if err != nil {
		return "", fmt.Errorf("uploading inline photo: %w", err)
	}
	deletion := tgbotapi.DeleteMessageConfig{
		ChatID: config.ChatID, ChannelUsername: config.ChannelUsername,
		MessageID: message.MessageID,
	}
	if _, err := bot.Request(deletion); err != nil {
		log.Printf("deleting inline cache message: %v", err)
	}
	if len(message.Photo) == 0 {
		return "", fmt.Errorf("inline upload returned no photo")
	}
	return message.Photo[len(message.Photo)-1].FileID, nil
}
