package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func richTextArtworkLink(raw json.RawMessage) *url.URL {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil
		}
		for _, item := range items {
			if link := richTextArtworkLink(item); link != nil {
				return link
			}
		}
	case '{':
		var text struct {
			Type string          `json:"type"`
			URL  string          `json:"url"`
			Text json.RawMessage `json:"text"`
		}
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil
		}
		if text.Type == "url" {
			if link := inlineLink(text.URL); link != nil {
				return link
			}
		}
		return richTextArtworkLink(text.Text)
	}
	return nil
}

func inlineMessageArtwork(message *telegramMessage) *url.URL {
	for _, part := range []*tgbotapi.Message{
		&message.Message,
		{Text: message.Caption, Entities: message.CaptionEntities},
	} {
		for _, link := range messageLinks(part) {
			if link = inlineLink(link.String()); link != nil {
				return link
			}
		}
	}
	for _, block := range message.RichMessage.Blocks {
		if link := richTextArtworkLink(block.Caption.Text); link != nil {
			return link
		}
	}
	return nil
}

func artworkActionData(action string, user int64, link *url.URL) string {
	id := artworkID(link)
	site, rawID, _ := strings.Cut(id, ":")
	number, _ := strconv.ParseUint(rawID, 10, 64)
	data := fmt.Sprintf("art:%s:%s:%s%s", action, strconv.FormatInt(user, 36), site[:1], strconv.FormatUint(number, 36))
	if site == "x" {
		name := strings.Split(link.Path, "/")[1]
		encoded := base64.RawURLEncoding.EncodeToString([]byte(name))
		if len(data)+1+len(encoded) > 64 {
			// FxTwitter identifies posts by ID and ignores the screen name.
			encoded = base64.RawURLEncoding.EncodeToString([]byte("i"))
		}
		data += ":" + encoded
	}
	return data
}

func artworkActionLink(parts []string) *url.URL {
	if len(parts) < 4 || len(parts[3]) < 2 {
		return nil
	}
	id, err := strconv.ParseUint(parts[3][1:], 36, 64)
	if err != nil || id == 0 {
		return nil
	}
	switch {
	case parts[3][0] == 'p' && len(parts) == 4:
		return inlineLink(fmt.Sprintf("https://www.pixiv.net/artworks/%d", id))
	case parts[3][0] == 'x' && len(parts) == 5:
		name, err := base64.RawURLEncoding.DecodeString(parts[4])
		if err != nil {
			return nil
		}
		link := &url.URL{Scheme: "https", Host: "x.com", Path: fmt.Sprintf("/%s/status/%d", name, id)}
		return inlineLink(link.String())
	}
	return nil
}

func handleIncomingMessage(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, message *telegramMessage, queue *mediaQueue, previews *thumbnails) {
	if message.ViaBot == nil || message.ViaBot.ID != bot.Self.ID {
		handleMessage(bot, app, db, &message.Message, queue, previews)
		return
	}
	if message.From == nil || message.Chat == nil {
		return
	}

	const question = "why did you send this to me?"
	reply := tgbotapi.NewMessage(message.Chat.ID, question)
	reply.ReplyToMessageID = message.MessageID
	link := inlineMessageArtwork(message)
	if link == nil {
		reply.Text += "\n\nCould not find the artwork link."
	} else {
		reply.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("remove from the history", artworkActionData("remove", message.From.ID, link))),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("get full-res version", artworkActionData("original", message.From.ID, link))),
		)
	}
	if _, err := bot.Send(reply); err != nil {
		log.Printf("replying to inline artwork: %v", err)
	}
}

func handleIncomingCallback(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, query *tgbotapi.CallbackQuery, queue *mediaQueue) {
	if strings.HasPrefix(query.Data, "art:") {
		handleArtworkCallback(bot, app, db, query, queue)
		return
	}
	handleCallback(bot, db, query)
}

func handleArtworkCallback(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, query *tgbotapi.CallbackQuery, queue *mediaQueue) {
	text, result := startArtworkAction(bot, app, db, query, queue)
	if _, err := bot.Request(tgbotapi.NewCallback(query.ID, text)); err != nil {
		log.Printf("answering artwork callback: %v", err)
	}
	if result == nil {
		return
	}
	if err := <-result; err != nil {
		log.Printf("sending original artwork: %v", err)
		reply := tgbotapi.NewMessage(query.Message.Chat.ID, "Could not get the full-res version. Please try again.")
		reply.ReplyToMessageID = query.Message.MessageID
		if _, err := bot.Send(reply); err != nil {
			log.Printf("sending original artwork failure: %v", err)
		}
	}
}

func startArtworkAction(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, query *tgbotapi.CallbackQuery, queue *mediaQueue) (string, <-chan error) {
	parts := strings.Split(query.Data, ":")
	if len(parts) < 3 || query.From == nil {
		return "Invalid request.", nil
	}
	base := 36
	if len(parts) == 3 { // Buttons sent before the source moved into callback data.
		base = 10
	}
	user, err := strconv.ParseInt(parts[2], base, 64)
	if err != nil || user != query.From.ID {
		return "This button belongs to another user.", nil
	}
	if query.Message == nil || query.Message.Chat == nil {
		return "Message unavailable.", nil
	}
	link := artworkActionLink(parts)
	if len(parts) == 3 {
		link = inlineMessageArtwork(&telegramMessage{Message: *query.Message})
	}
	if link == nil {
		return "Could not find the artwork link.", nil
	}

	switch parts[1] {
	case "remove":
		removed, err := removeHistoryArtwork(db, user, artworkID(link))
		if err != nil {
			log.Printf("removing artwork from history: %v", err)
			return "Could not remove the link. Please try again.", nil
		}
		if removed {
			return "Removed from your history.", nil
		}
		return "This link is not in your history.", nil
	case "original":
		saved, err := findArtwork(db, artworkID(link))
		if err != nil {
			log.Printf("loading artwork for original files: %v", err)
			return "Could not load the artwork. Please try again.", nil
		}
		result, err := queue.submit(user, func() error {
			stop := showProcessing(bot, query.Message.Chat.ID, tgbotapi.ChatUploadDocument)
			defer stop()
			return sendOriginalFiles(bot, app, query.Message.Chat.ID, query.Message.MessageID, link, saved)
		})
		if err != nil {
			return fmt.Sprintf("Could not get the full-res version: %s. Please retry later.", err), nil
		}
		return "", result
	default:
		return "Invalid request.", nil
	}
}
