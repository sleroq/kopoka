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
		if count == 0 {
			text = "Your history is empty. Use /historyon to turn history back on."
			keyboard = tgbotapi.NewInlineKeyboardMarkup()
		} else {
			text = fmt.Sprintf("Are you sure you want to delete your %d saved links?", count)
			keyboard = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
				historyButton("Confirm", "confirm", user), historyButton("Cancel", "cancel", user)))
		}
	case "confirm":
		if err := deleteHistory(db, user); err != nil {
			log.Printf("deleting history: %v", err)
			answer.Text = "Could not delete your history. Please try again."
			return
		}
		text = "Your history has been deleted. Use /historyon to turn history back on."
		keyboard = tgbotapi.NewInlineKeyboardMarkup()
	case "cancel":
		count, err := historyCount(db, user)
		if err != nil {
			log.Printf("counting history: %v", err)
			answer.Text = "Could not count your history. Please try again."
			return
		}
		if count == 0 {
			keyboard = tgbotapi.NewInlineKeyboardMarkup()
		}
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

func settingsView(db *sql.DB, user int64) (string, tgbotapi.InlineKeyboardMarkup, error) {
	var disabled, hidden bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_opt_out WHERE user_id=?),
        EXISTS(SELECT 1 FROM history_chat_hidden WHERE user_id=?)`, user, user).Scan(&disabled, &hidden)
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	history, chat := "On", "✓"
	if disabled {
		history = "Off"
	}
	if hidden {
		chat = "✗"
	}
	button := func(text, action string) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("settings:%s:%d", action, user))
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button("History: "+history, "history")),
		tgbotapi.NewInlineKeyboardRow(button(chat+" Show chat links in search history", "chat")),
	)
	return "Settings", keyboard, nil
}

func showSettings(bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message) {
	text, keyboard, err := settingsView(db, message.From.ID)
	if err != nil {
		log.Printf("loading settings: %v", err)
		text = "Could not load your settings. Please try again."
	}
	reply := tgbotapi.NewMessage(message.Chat.ID, text)
	reply.ReplyToMessageID = message.MessageID
	if err == nil {
		reply.ReplyMarkup = keyboard
	}
	if _, err := bot.Send(reply); err != nil {
		log.Printf("sending settings: %v", err)
	}
}

func handleSettingsCallback(bot *tgbotapi.BotAPI, db *sql.DB, query *tgbotapi.CallbackQuery) {
	answer := tgbotapi.NewCallback(query.ID, "")
	defer func() {
		if _, err := bot.Request(answer); err != nil {
			log.Printf("answering settings callback: %v", err)
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

	switch parts[1] {
	case "history":
		var disabled bool
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_opt_out WHERE user_id=?)`, user).Scan(&disabled)
		if err == nil {
			err = setHistoryEnabled(db, user, disabled)
		}
	case "chat":
		var hidden bool
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_chat_hidden WHERE user_id=?)`, user).Scan(&hidden)
		if err == nil {
			if hidden {
				_, err = db.Exec(`DELETE FROM history_chat_hidden WHERE user_id=?`, user)
			} else {
				_, err = db.Exec(`INSERT INTO history_chat_hidden(user_id) VALUES (?)`, user)
			}
		}
	default:
		answer.Text = "Invalid request."
		return
	}
	if err != nil {
		log.Printf("changing settings: %v", err)
		answer.Text = "Could not change your settings. Please try again."
		return
	}

	text, keyboard, err := settingsView(db, user)
	if err != nil {
		log.Printf("loading settings: %v", err)
		answer.Text = "Could not load your settings. Please try again."
		return
	}
	edit := tgbotapi.NewEditMessageText(query.Message.Chat.ID, query.Message.MessageID, text)
	edit.ReplyMarkup = &keyboard
	if _, err := bot.Request(edit); err != nil {
		log.Printf("editing settings: %v", err)
		answer.Text = "Could not update the message. Please try again."
	}
}
