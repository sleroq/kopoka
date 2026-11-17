package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

type translationSettings struct {
	Target  string
	Ignored []string
}

func defaultTranslationSettings() translationSettings {
	return translationSettings{Target: "en", Ignored: []string{"ru"}}
}

func loadTranslationSettings(db *sql.DB, user int64) (translationSettings, error) {
	var s translationSettings
	var ignored string
	err := db.QueryRow(`SELECT target, ignored FROM user_translation WHERE user_id=?`, user).Scan(&s.Target, &ignored)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultTranslationSettings(), nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(ignored), &s.Ignored)
	return s, err
}

func (s translationSettings) key() string {
	ignored := slices.Clone(s.Ignored)
	slices.Sort(ignored)
	return s.Target + ":" + strings.Join(ignored, ",")
}

func (s translationSettings) ignores(code string) bool {
	primary, _, _ := strings.Cut(strings.ToLower(code), "-")
	return slices.Contains(s.Ignored, primary)
}

type translationLanguage struct{ code, name string }

var translationLanguages = buildTranslationLanguages()

func buildTranslationLanguages() []translationLanguage {
	var result []translationLanguage
	for _, base := range language.Supported.BaseLanguages() {
		code := base.String()
		if len(code) == 2 {
			result = append(result, translationLanguage{code, display.English.Languages().Name(base)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result
}

func languageName(code string) string {
	primary, _, _ := strings.Cut(strings.ToLower(code), "-")
	for _, entry := range translationLanguages {
		if entry.code == primary {
			return entry.name
		}
	}
	return code
}

func validTranslationLanguage(code string) bool {
	for _, entry := range translationLanguages {
		if entry.code == code {
			return true
		}
	}
	return false
}

func translationQuery(query string) (string, string, bool) {
	fields := strings.Fields(query)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "language") {
		return "", "", false
	}
	mode := strings.ToLower(fields[1])
	if mode != "target" && mode != "ignore" {
		return "", "", false
	}
	return mode, strings.Join(fields[2:], " "), true
}

func translationKeyboard(mode string) tgbotapi.InlineKeyboardMarkup {
	query := "language " + mode + " "
	text := "Change target language"
	if mode == "ignore" {
		text = "Change ignored languages"
	}
	button := tgbotapi.InlineKeyboardButton{Text: text, SwitchInlineQueryCurrentChat: &query}
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(button))
}

func isTranslationSelection(message *tgbotapi.Message) bool {
	if message.ReplyMarkup == nil {
		return false
	}
	for _, row := range message.ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if button.SwitchInlineQueryCurrentChat != nil && (*button.SwitchInlineQueryCurrentChat == "language target " || *button.SwitchInlineQueryCurrentChat == "language ignore ") {
				return true
			}
		}
	}
	return false
}

func translationResults(db *sql.DB, query *tgbotapi.InlineQuery, mode, filter string) ([]any, string) {
	settings, err := loadTranslationSettings(db, query.From.ID)
	if err != nil {
		log.Printf("loading translation settings: %v", err)
		return []any{}, ""
	}

	var matches []translationLanguage
	for _, entry := range translationLanguages {
		search := strings.ToLower(entry.name + " " + entry.code)
		matched := true
		for token := range strings.FieldsSeq(strings.ToLower(filter)) {
			if !strings.Contains(search, token) {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, entry)
		}
	}

	offset, err := strconv.Atoi(query.Offset)
	if err != nil || offset < 0 || offset > len(matches) {
		offset = 0
	}
	end := min(offset+50, len(matches))
	results := make([]any, 0, end-offset)
	keyboard := translationKeyboard(mode)

	for _, entry := range matches[offset:end] {
		title, description := entry.name, "Target language: "+languageName(settings.Target)
		if mode == "target" && entry.code == settings.Target {
			title = "✓ " + title
		}
		if mode == "ignore" {
			if settings.ignores(entry.code) {
				title = "✓ " + title
				description = "Translate"
			} else {
				title = "○ " + title
				description = "Ignore"
			}
		}
		id := "language:" + mode + ":" + entry.code
		article := tgbotapi.NewInlineQueryResultArticle(id, title, "Changing translation settings…")
		article.Description = description
		article.ReplyMarkup = &keyboard
		results = append(results, article)
	}

	next := ""
	if end < len(matches) {
		next = strconv.Itoa(end)
	}
	return results, next
}

func saveTranslationSelection(db *sql.DB, user int64, mode, code string) (translationSettings, error) {
	settings, err := loadTranslationSettings(db, user)
	if err != nil {
		return settings, err
	}
	if mode == "target" {
		settings.Target = code
		_, err = db.Exec(`INSERT INTO user_translation(user_id,target) VALUES (?,?)
   ON CONFLICT(user_id) DO UPDATE SET target=excluded.target`, user, code)
		return settings, err
	}
	if settings.ignores(code) {
		settings.Ignored = slices.DeleteFunc(settings.Ignored, func(item string) bool { return item == code })
	} else {
		settings.Ignored = append(settings.Ignored, code)
	}
	encoded, err := json.Marshal(settings.Ignored)
	if err != nil {
		return settings, err
	}
	_, err = db.Exec(`INSERT INTO user_translation(user_id,ignored) VALUES (?,?)
  ON CONFLICT(user_id) DO UPDATE SET ignored=excluded.ignored`, user, string(encoded))
	return settings, err
}

func handleTranslationSelection(bot *tgbotapi.BotAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult) {
	parts := strings.Split(chosen.ResultID, ":")
	if chosen.From == nil || len(parts) != 3 || parts[0] != "language" || (parts[1] != "target" && parts[1] != "ignore") || !validTranslationLanguage(parts[2]) {
		return
	}

	mode, code := parts[1], parts[2]
	settings, err := saveTranslationSelection(db, chosen.From.ID, mode, code)
	message := "Target language set to " + languageName(code) + "."
	if mode == "ignore" {
		names := make([]string, 0, len(settings.Ignored))
		for _, ignored := range settings.Ignored {
			names = append(names, languageName(ignored))
		}
		if len(names) == 0 {
			message = "Ignored languages: None."
		} else {
			message = "Ignored languages: " + strings.Join(names, ", ") + "."
		}
	}
	if err != nil {
		log.Printf("saving translation settings: %v", err)
		message = "Could not save translation settings. Please try again."
	}
	if chosen.InlineMessageID == "" {
		return
	}

	keyboard := translationKeyboard(mode)
	edit := tgbotapi.NewEditMessageText(0, 0, message)
	edit.InlineMessageID = chosen.InlineMessageID
	edit.ReplyMarkup = &keyboard
	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) { return bot.Request(edit) }); err != nil {
		log.Printf("editing translation confirmation: %v", err)
	}
}
