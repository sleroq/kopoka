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

func settingsView(user int64) (string, tgbotapi.InlineKeyboardMarkup) {
	button := func(text, action string) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("settings:%s:%d", action, user))
	}
	query := "tz "
	targetQuery := "language target "
	ignoreQuery := "language ignore "
	return "Settings", tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button("Configure history", "history-menu")),
		tgbotapi.NewInlineKeyboardRow(button("Instant inline query", "instant")),
		tgbotapi.NewInlineKeyboardRow(button("Specific photo links", "photo-menu")),
		tgbotapi.NewInlineKeyboardRow(button("X post format", "x-format-menu")),
		tgbotapi.NewInlineKeyboardRow(button("Post image themes", "theme-menu")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.InlineKeyboardButton{Text: "Target language", SwitchInlineQueryCurrentChat: &targetQuery}),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.InlineKeyboardButton{Text: "Ignore translations", SwitchInlineQueryCurrentChat: &ignoreQuery}),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.InlineKeyboardButton{Text: "Timezone", SwitchInlineQueryCurrentChat: &query}),
	)
}

func historySettingsView(db *sql.DB, user int64, username string) (string, tgbotapi.InlineKeyboardMarkup, error) {
	var disabled, hidden, firstSaved, pictures bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM history_opt_out WHERE user_id=?),
        EXISTS(SELECT 1 FROM history_chat_hidden WHERE user_id=?),
        EXISTS(SELECT 1 FROM history_first_saved WHERE user_id=?),
        EXISTS(SELECT 1 FROM history_picture_view WHERE user_id=?)`, user, user, user, user).Scan(&disabled, &hidden, &firstSaved, &pictures)
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	chat := "✓"
	if hidden {
		chat = "✗"
	}
	order := "Recent activity"
	if firstSaved {
		order = "First saved"
	}
	view := "Posts"
	if pictures {
		view = "Pictures"
	}
	button := func(text, action string) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("settings:%s:%d", action, user))
	}
	history := tgbotapi.NewInlineKeyboardButtonURL("Disable history", "https://t.me/"+username+"?text=%2Fhistoryoff")
	if disabled {
		history = button("Enable history", "enable-history")
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button("History order: "+order, "order")),
		tgbotapi.NewInlineKeyboardRow(button("Default view: "+view, "view")),
		tgbotapi.NewInlineKeyboardRow(button("Show chat links in search history: "+chat, "chat")),
		tgbotapi.NewInlineKeyboardRow(history),
		tgbotapi.NewInlineKeyboardRow(button("Back", "back")),
	)
	return "Settings\n\nHistory\nUse . for the other view.", keyboard, nil
}

func instantSettingsView(db *sql.DB, user int64) (string, tgbotapi.InlineKeyboardMarkup, error) {
	settings, err := loadInstantInlineSettings(db, user)
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	button := func(text, action string) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("settings:%s:%d", action, user))
	}
	mark := func(enabled bool) string {
		if enabled {
			return "✓"
		}
		return "✗"
	}
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button("Toggle all", "instant-all")),
		tgbotapi.NewInlineKeyboardRow(button("X: "+mark(settings.X), "instant-x")),
		tgbotapi.NewInlineKeyboardRow(button("Pixiv: "+mark(settings.Pixiv), "instant-pixiv")),
		tgbotapi.NewInlineKeyboardRow(button("Back", "back")),
	)
	text := "Settings\n\n<blockquote><b>Instant inline query</b>\n" +
		"Skip external previews for new links to show inline results as quickly as possible.</blockquote>"
	return text, keyboard, nil
}

func photoSettingsView(db *sql.DB, user int64) (string, tgbotapi.InlineKeyboardMarkup, error) {
	var disabled bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM photo_link_opt_out WHERE user_id=?)`, user).Scan(&disabled)
	mark := "Enabled"
	if disabled {
		mark = "Disabled"
	}
	button := func(text, action string) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("settings:%s:%d", action, user))
	}
	return "Settings\n\nSpecific photo links", tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button(mark, "photo-toggle")),
		tgbotapi.NewInlineKeyboardRow(button("Back", "back"))), err
}

func xPostFormatSettingsView(db *sql.DB, user int64) (string, tgbotapi.InlineKeyboardMarkup, error) {
	enabled, err := xPostRichTextEnabled(db, user)
	format := "Telegram text"
	if enabled {
		format = "Rich text"
	}
	button := func(text, action string) tgbotapi.InlineKeyboardButton {
		return tgbotapi.NewInlineKeyboardButtonData(text, fmt.Sprintf("settings:%s:%d", action, user))
	}
	return "Settings\n\nX post format", tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button(format, "x-format-toggle")),
		tgbotapi.NewInlineKeyboardRow(button("Back", "back"))), err
}

func showSettings(bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message) {
	text, keyboard := settingsView(message.From.ID)
	reply := tgbotapi.NewMessage(message.Chat.ID, text)
	reply.ReplyToMessageID = message.MessageID
	reply.ReplyMarkup = keyboard
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

	view := parts[1]
	switch parts[1] {
	case "instant", "history-menu", "photo-menu", "theme-menu", "x-format-menu":
	case "x-format-toggle":
		view = "x-format-menu"
		err = toggleXPostFormat(db, user)
	case "photo-toggle":
		view = "photo-menu"
		err = togglePhotoLinks(db, user)
	case "instant-all", "instant-x", "instant-pixiv":
		view = "instant"
		err = toggleInstantInline(db, user, strings.TrimPrefix(parts[1], "instant-"))
	case "back":
	case "enable-history":
		view = "history-menu"
		err = setHistoryEnabled(db, user, true)
	case "history", "order", "chat":
		view = "history-menu"
		err = toggleHistorySetting(db, user, parts[1])
	case "view":
		view = "history-menu"
		err = togglePictureHistory(db, user)
	default:
		answer.Text = "Invalid request."
		return
	}
	if err != nil {
		log.Printf("changing settings: %v", err)
		answer.Text = "Could not change your settings. Please try again."
		return
	}

	var text string
	var keyboard tgbotapi.InlineKeyboardMarkup
	switch view {
	case "theme-menu":
		text, keyboard = themeSettingsView(user)
	case "photo-menu":
		text, keyboard, err = photoSettingsView(db, user)
	case "x-format-menu":
		text, keyboard, err = xPostFormatSettingsView(db, user)
	case "instant":
		text, keyboard, err = instantSettingsView(db, user)
	case "history-menu":
		text, keyboard, err = historySettingsView(db, user, bot.Self.UserName)
	default:
		text, keyboard = settingsView(user)
	}
	if err != nil {
		log.Printf("loading settings: %v", err)
		answer.Text = "Could not load your settings. Please try again."
		return
	}
	edit := tgbotapi.NewEditMessageText(query.Message.Chat.ID, query.Message.MessageID, text)
	edit.ReplyMarkup = &keyboard
	edit.ParseMode = "HTML"
	if _, err := bot.Request(edit); err != nil {
		log.Printf("editing settings: %v", err)
		answer.Text = "Could not update the message. Please try again."
	}
}
