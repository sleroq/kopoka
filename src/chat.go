package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func sendChatJSON(bot *tgbotapi.BotAPI, message *tgbotapi.Message) error {
	response, err := bot.MakeRequest("getChat", tgbotapi.Params{"chat_id": strconv.FormatInt(message.Chat.ID, 10)})
	if err != nil {
		return fmt.Errorf("fetching chat: %w", err)
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, response.Result, "", "  "); err != nil {
		return fmt.Errorf("formatting chat JSON: %w", err)
	}
	if formatted.Len() > 4096 {
		doc := tgbotapi.NewDocument(message.Chat.ID, tgbotapi.FileBytes{Name: "chat.json", Bytes: formatted.Bytes()})
		doc.ReplyToMessageID = message.MessageID
		_, err = bot.Send(doc)
	} else {
		reply := tgbotapi.NewMessage(message.Chat.ID, "<pre>"+html.EscapeString(formatted.String())+"</pre>")
		reply.ParseMode = "HTML"
		reply.ReplyToMessageID = message.MessageID
		_, err = bot.Send(reply)
	}
	return err
}
