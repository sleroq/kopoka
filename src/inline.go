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

const inlineResultID = "artwork"

func inlineLink(text string) *url.URL {
	link, err := url.Parse(strings.TrimSpace(text))
	if err != nil || (link.Scheme != "https" && link.Scheme != "http") || !supportedLink(link) || link.User != nil {
		return nil
	}
	if link.Host == "pixiv.net" || link.Host == "www.pixiv.net" {
		id, ok := strings.CutPrefix(link.Path, "/artworks/")
		n, err := strconv.ParseUint(id, 10, 64)
		if !ok || err != nil || n == 0 {
			return nil
		}
	} else {
		parts := strings.Split(strings.TrimPrefix(link.Path, "/"), "/")
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

func handleInlineQuery(bot *tgbotapi.BotAPI, query *tgbotapi.InlineQuery) {
	log.Print("received inline query")
	results := []any{}
	if inlineLink(query.Query) != nil {
		results = append(results, map[string]any{
			"type": "article", "id": inlineResultID, "title": "Artwork",
			"input_message_content": map[string]any{"message_text": "Loading artwork…", "disable_web_page_preview": true},
			"reply_markup":          map[string]any{"inline_keyboard": []any{[]any{map[string]any{"text": "Please wait…", "disabled": map[string]any{}}}}},
		})
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		log.Printf("encoding inline results: %v", err)
		return
	}
	params := tgbotapi.Params{
		"inline_query_id": query.ID, "results": string(encoded), "cache_time": "0",
	}
	if _, err := bot.MakeRequest("answerInlineQuery", params); err != nil {
		log.Printf("answering inline query: %v", err)
	}
}

func handleChosenInlineResult(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, chosen *tgbotapi.ChosenInlineResult, cacheChat string) {
	log.Print("received chosen inline result")
	if chosen.ResultID != inlineResultID || chosen.InlineMessageID == "" {
		log.Print("invalid chosen inline result or missing inline message ID")
		return
	}
	if err := editInlineArtwork(bot, app, chosen, cacheChat); err != nil {
		log.Printf("processing chosen inline result: %v", err)
		params := tgbotapi.Params{
			"inline_message_id": chosen.InlineMessageID, "text": "Could not load artwork. Please try again.",
			"reply_markup": `{"inline_keyboard":[]}`,
		}
		if _, editErr := retryTelegram(func() (*tgbotapi.APIResponse, error) {
			return bot.MakeRequest("editMessageText", params)
		}); editErr != nil {
			log.Printf("editing inline failure message: %v", editErr)
		}
	}
}

func editInlineArtwork(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, chosen *tgbotapi.ChosenInlineResult, cacheChat string) error {
	link := inlineLink(chosen.Query)
	if link == nil {
		return fmt.Errorf("invalid artwork URL")
	}

	photos, err := fetchArtPhotos(app, link)
	if err != nil {
		return err
	}
	if len(photos) == 0 {
		return fmt.Errorf("artwork has no photos")
	}
	if len(photos) > 50 {
		return fmt.Errorf("inline slideshow exceeds 50 photos: %d", len(photos))
	}

	ids := make([]string, 0, len(photos))
	for _, item := range photos {
		id, err := cacheInlinePhoto(bot, cacheChat, item.(tgbotapi.InputMediaPhoto))
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}

	first := photos[0].(tgbotapi.InputMediaPhoto)
	params := tgbotapi.Params{"inline_message_id": chosen.InlineMessageID, "reply_markup": `{"inline_keyboard":[]}`}
	method := "editMessageMedia"
	var payload any
	if len(ids) == 1 {
		payload = map[string]any{"type": "photo", "media": ids[0], "caption": first.Caption, "parse_mode": first.ParseMode}
	} else {
		method = "editMessageText"
		var markup strings.Builder
		markup.WriteString("<tg-slideshow>")
		media := make([]any, 0, len(ids))
		for i, id := range ids {
			key := strconv.Itoa(i)
			fmt.Fprintf(&markup, `<img src="tg://photo?id=%s"/>`, key)
			media = append(media, map[string]any{"id": key, "media": map[string]any{"type": "photo", "media": id}})
		}
		fmt.Fprintf(&markup, "<figcaption>%s</figcaption></tg-slideshow>", first.Caption)
		payload = map[string]any{"html": markup.String(), "media": media}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding inline artwork: %w", err)
	}
	if len(ids) == 1 {
		params["media"] = string(encoded)
	} else {
		params["rich_message"] = string(encoded)
	}
	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) {
		return bot.MakeRequest(method, params)
	}); err != nil {
		return fmt.Errorf("editing inline artwork: %w", err)
	}
	return nil
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
	message, err := retryTelegram(func() (tgbotapi.Message, error) {
		attempt := tgbotapi.NewPhoto(config.ChatID, photo.Media)
		attempt.ChannelUsername = config.ChannelUsername
		return bot.Send(attempt)
	})
	if err != nil {
		return "", fmt.Errorf("uploading inline photo: %w", err)
	}
	deletion := tgbotapi.DeleteMessageConfig{
		ChatID: config.ChatID, ChannelUsername: config.ChannelUsername,
		MessageID: message.MessageID,
	}
	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) {
		return bot.Request(deletion)
	}); err != nil {
		log.Printf("deleting inline cache message: %v", err)
	}
	if len(message.Photo) == 0 {
		return "", fmt.Errorf("inline upload returned no photo")
	}
	return message.Photo[len(message.Photo)-1].FileID, nil
}
