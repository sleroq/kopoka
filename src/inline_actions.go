package main

import (
	"bytes"
	"database/sql"
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
		// Keep the source with the question so callbacks also work before a new
		// inline result has finished loading or when history is disabled.
		reply.Entities = []tgbotapi.MessageEntity{{Type: "text_link", Offset: 0, Length: len(question), URL: link.String()}}
		reply.DisableWebPagePreview = true
		user := strconv.FormatInt(message.From.ID, 10)
		reply.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("remove from the history", "art:remove:"+user)),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("get full-res version", "art:original:"+user)),
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
	if len(parts) != 3 || query.From == nil {
		return "Invalid request.", nil
	}
	user, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || user != query.From.ID {
		return "This button belongs to another user.", nil
	}
	if query.Message == nil || query.Message.Chat == nil {
		return "Message unavailable.", nil
	}
	link := inlineMessageArtwork(&telegramMessage{Message: *query.Message})
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
		result, err := queue.submit(user, func() error {
			stop := showProcessing(bot, query.Message.Chat.ID, tgbotapi.ChatUploadDocument)
			defer stop()
			return sendOriginalFiles(bot, app, query.Message.Chat.ID, query.Message.MessageID, link)
		})
		if err != nil {
			return fmt.Sprintf("Could not get the full-res version: %s. Please retry later.", err), nil
		}
		return "", result
	default:
		return "Invalid request.", nil
	}
}
