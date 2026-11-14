package main

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const historyOffText = "History is off. Use /historyon to turn it back on."

func historyButton(text, action string, user int64) tgbotapi.InlineKeyboardButton {
	return tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("history:%s:%d", action, user))
}

func historyDeleteKeyboard(user int64) tgbotapi.InlineKeyboardMarkup {
	button := historyButton("Delete my history", "delete", user)
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(button))
}

func handleHistoryCallback(bot *tgbotapi.BotAPI, db *sql.DB, query *tgbotapi.CallbackQuery) {
	answer := tgbotapi.NewCallback(query.ID, "")
	defer func() {
		if _, err := bot.Request(answer); err != nil {
			log.Printf("answering history callback: %v", err)
		}
	}()

	parts := strings.Split(query.Data, ":")
	if len(parts) != 3 || query.From == nil {
		answer.Text = "Invalid request."
		return
	}

	user, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || user != query.From.ID {
		answer.Text = "This button belongs to another user."
		return
	}
	if query.Message == nil || query.Message.Chat == nil {
		answer.Text = "Message unavailable."
		return
	}

	text := historyOffText
	keyboard := historyDeleteKeyboard(user)
	switch parts[1] {
	case "delete":
		count, err := historyCount(db, user)
		if err != nil {
			log.Printf("counting history: %v", err)
			answer.Text = "Could not count your history. Please try again."
			return
		}
		text = fmt.Sprintf("Are you sure you want to delete your %d saved links?", count)
		keyboard = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			historyButton("Confirm", "confirm", user), historyButton("Cancel", "cancel", user)))
	case "confirm":
		if err := deleteHistory(db, user); err != nil {
			log.Printf("deleting history: %v", err)
			answer.Text = "Could not delete your history. Please try again."
			return
		}
		text = "Your history has been deleted. Use /historyon to turn history back on."
	case "cancel":
	default:
		answer.Text = "Invalid request."
		return
	}

	edit := tgbotapi.NewEditMessageText(query.Message.Chat.ID, query.Message.MessageID, text)
	edit.ReplyMarkup = &keyboard
	if _, err := bot.Request(edit); err != nil {
		log.Printf("editing history reply: %v", err)
		answer.Text = "Could not update the message. Please try again."
	}
}
