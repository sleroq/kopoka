package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type postImageThemeOption struct{ ID, Name, Family string }

var postImageThemeCatalog = []postImageThemeOption{
	{"light", "Light", "light"},
	{"warm", "Warm", "light"},
	{"dark", "Dark", "dark"},
	{"dim", "Dim", "dark"},
}

func themeOption(id string) (postImageThemeOption, bool) {
	for _, option := range postImageThemeCatalog {
		if option.ID == id {
			return option, true
		}
	}
	return postImageThemeOption{}, false
}

type postImageThemes struct{ Light, Dark string }

func loadPostImageThemes(db *sql.DB, user int64) (postImageThemes, error) {
	themes := postImageThemes{Light: "light", Dark: defaultPostImageTheme}
	err := db.QueryRow(`SELECT light, dark FROM post_image_themes WHERE user_id=?`, user).Scan(&themes.Light, &themes.Dark)
	if err == sql.ErrNoRows {
		err = nil
	}
	return themes, err
}

func savePostImageTheme(db *sql.DB, user int64, theme string) error {
	option, ok := themeOption(theme)
	if !ok {
		return fmt.Errorf("invalid post image theme: %s", theme)
	}
	query := `INSERT INTO post_image_themes(user_id, light) VALUES (?, ?)
        ON CONFLICT(user_id) DO UPDATE SET light=excluded.light`
	if option.Family == "dark" {
		query = `INSERT INTO post_image_themes(user_id, dark) VALUES (?, ?)
        ON CONFLICT(user_id) DO UPDATE SET dark=excluded.dark`
	}
	_, err := db.Exec(query, user, theme)
	return err
}

func themeSettingsView(user int64) (string, tgbotapi.InlineKeyboardMarkup) {
	light, dark := "theme light ", "theme dark "
	return "Settings\n\nPost image themes", tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.InlineKeyboardButton{Text: "Light theme", SwitchInlineQueryCurrentChat: &light}),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.InlineKeyboardButton{Text: "Dark theme", SwitchInlineQueryCurrentChat: &dark}),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Back", fmt.Sprintf("settings:back:%d", user))),
	)
}

func themeQuery(query string) (string, string, bool) {
	fields := strings.Fields(query)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "theme") {
		return "", "", false
	}
	family := strings.ToLower(fields[1])
	if family != "light" && family != "dark" {
		return "", "", false
	}
	return family, strings.ToLower(strings.Join(fields[2:], " ")), true
}

func themeKeyboard(family string) tgbotapi.InlineKeyboardMarkup {
	query := "theme " + family + " "
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.InlineKeyboardButton{Text: "Change theme again", SwitchInlineQueryCurrentChat: &query}))
}

func isThemeSelection(message *tgbotapi.Message) bool {
	if message.ReplyMarkup == nil {
		return false
	}
	for _, row := range message.ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if button.SwitchInlineQueryCurrentChat != nil && (*button.SwitchInlineQueryCurrentChat == "theme light " || *button.SwitchInlineQueryCurrentChat == "theme dark ") {
				return true
			}
		}
	}
	return false
}

func themeResults(db *sql.DB, user int64, family, filter string, previews *thumbnails) []any {
	themes, err := loadPostImageThemes(db, user)
	if err != nil {
		log.Printf("loading post image themes: %v", err)
		return []any{}
	}
	selected := themes.Light
	if family == "dark" {
		selected = themes.Dark
	}
	results := []any{}
	keyboard := themeKeyboard(family)
	for _, option := range postImageThemeCatalog {
		if option.Family != family || !strings.Contains(strings.ToLower(option.Name), filter) {
			continue
		}
		result := inlinePhoto(previews.themePreview(option.ID, option.ID == selected))
		result["id"], result["title"] = "theme:"+option.ID, option.Name
		result["input_message_content"] = map[string]any{"message_text": "Setting theme…"}
		result["reply_markup"] = keyboard
		results = append(results, result)
	}
	return results
}

func handleThemeSelection(bot *tgbotapi.BotAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult) {
	if chosen.From == nil {
		return
	}
	option, ok := themeOption(strings.TrimPrefix(chosen.ResultID, "theme:"))
	if !ok {
		return
	}
	message := option.Name + " theme selected."
	if err := savePostImageTheme(db, chosen.From.ID, option.ID); err != nil {
		log.Printf("saving post image theme: %v", err)
		message = "Could not save theme. Please try again."
	}
	if chosen.InlineMessageID == "" {
		return
	}
	keyboard := themeKeyboard(option.Family)
	edit := tgbotapi.NewEditMessageText(0, 0, message)
	edit.InlineMessageID, edit.ReplyMarkup = chosen.InlineMessageID, &keyboard
	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) { return bot.Request(edit) }); err != nil {
		log.Printf("editing theme confirmation: %v", err)
	}
}
