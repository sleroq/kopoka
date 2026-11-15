package main

import (
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const defaultTimezone = "Etc/GMT-3"
const timezoneResultPrefix = "timezone:"

//go:embed timezones/zone.tab
var zoneTable string

//go:embed timezones/iso3166.tab
var countryTable string

type timezoneEntry struct {
	id, country, code, comment, search string
}

var timezoneCatalog = buildTimezoneCatalog()

func buildTimezoneCatalog() []timezoneEntry {
	countries := map[string]string{}
	for line := range strings.SplitSeq(countryTable, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		countries[fields[0]] = fields[1]
	}
	var entries []timezoneEntry
	add := func(id, code, comment string) {
		country := countries[code]
		words := strings.NewReplacer("/", " ", "_", " ").Replace(id)
		search := strings.ToLower(strings.Join([]string{words, id, code, country, comment}, " "))
		entries = append(entries, timezoneEntry{id, country, code, comment, search})
	}
	for line := range strings.SplitSeq(zoneTable, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		comment := ""
		if len(fields) > 3 {
			comment = fields[3]
		}
		add(fields[2], fields[0], comment)
	}
	add("UTC", "", "")
	for offset := -12; offset <= 14; offset++ {
		if offset == 0 {
			continue
		}
		add(fmt.Sprintf("Etc/GMT%+d", -offset), "", "")
		entries[len(entries)-1].search = fmt.Sprintf("utc%+d gmt%+d utc%+03d:00 gmt%+03d:00", offset, offset, offset, offset)
	}
	return entries
}

func timezoneByID(id string) (timezoneEntry, bool) {
	for _, zone := range timezoneCatalog {
		if zone.id == id {
			return zone, true
		}
	}
	return timezoneEntry{}, false
}

func loadUserTimezone(db *sql.DB, user int64) (string, error) {
	var zone string
	err := db.QueryRow(`SELECT timezone FROM user_timezone WHERE user_id=?`, user).Scan(&zone)
	if err == sql.ErrNoRows {
		return defaultTimezone, nil
	}
	return zone, err
}

func saveUserTimezone(db *sql.DB, user int64, zone string) error {
	if _, ok := timezoneByID(zone); !ok {
		return fmt.Errorf("invalid timezone: %s", zone)
	}
	_, err := db.Exec(`INSERT INTO user_timezone(user_id, timezone) VALUES (?, ?)
        ON CONFLICT(user_id) DO UPDATE SET timezone=excluded.timezone`, user, zone)
	return err
}

func timezoneQuery(query string) (string, bool) {
	fields := strings.Fields(query)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "tz") {
		return "", false
	}
	return strings.Join(fields[1:], " "), true
}

func (zone timezoneEntry) title() string {
	location, err := time.LoadLocation(zone.id)
	if err != nil {
		return zone.id
	}
	_, seconds := time.Now().In(location).Zone()
	sign := "+"
	if seconds < 0 {
		sign, seconds = "-", -seconds
	}
	offset := fmt.Sprintf("UTC%s%02d:%02d", sign, seconds/3600, seconds%3600/60)
	if zone.id == "UTC" || strings.HasPrefix(zone.id, "Etc/GMT") {
		return offset
	}
	return strings.ReplaceAll(zone.id, "_", " ") + " (" + offset + ")"
}

func isTimezoneSelection(message *tgbotapi.Message) bool {
	if message.ReplyMarkup == nil {
		return false
	}
	for _, row := range message.ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if button.SwitchInlineQueryCurrentChat != nil && *button.SwitchInlineQueryCurrentChat == "tz " {
				return true
			}
		}
	}
	return false
}

func timezoneKeyboard() tgbotapi.InlineKeyboardMarkup {
	query := "tz "
	button := tgbotapi.InlineKeyboardButton{Text: "Change timezone", SwitchInlineQueryCurrentChat: &query}
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(button))
}

func (zone timezoneEntry) matches(tokens []string) bool {
	for _, token := range tokens {
		if !strings.Contains(zone.search, token) && !strings.Contains(strings.ToLower(zone.title()), token) {
			return false
		}
	}
	return true
}

func timezoneResults(query *tgbotapi.InlineQuery) ([]any, string) {
	filter, _ := timezoneQuery(query.Query)
	tokens := strings.Fields(strings.ToLower(filter))
	var matches []timezoneEntry
	for _, zone := range timezoneCatalog {
		if zone.matches(tokens) {
			matches = append(matches, zone)
		}
	}
	offset, err := strconv.Atoi(query.Offset)
	if err != nil || offset < 0 || offset > len(matches) {
		offset = 0
	}
	end := min(offset+50, len(matches))
	results := make([]any, 0, end-offset)
	keyboard := timezoneKeyboard()
	for _, zone := range matches[offset:end] {
		article := tgbotapi.NewInlineQueryResultArticle(timezoneResultPrefix+zone.id, zone.title(), "Setting timezone…")
		article.Description = zone.country
		if zone.comment != "" {
			article.Description += " — " + zone.comment
		}
		article.ReplyMarkup = &keyboard
		results = append(results, article)
	}
	next := ""
	if end < len(matches) {
		next = strconv.Itoa(end)
	}
	return results, next
}

func handleTimezoneSelection(bot *tgbotapi.BotAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult) {
	if chosen.From == nil || !strings.HasPrefix(chosen.ResultID, timezoneResultPrefix) {
		return
	}
	zone, ok := timezoneByID(strings.TrimPrefix(chosen.ResultID, timezoneResultPrefix))
	if !ok {
		return
	}
	message := "Timezone set to " + zone.title() + "."
	if err := saveUserTimezone(db, chosen.From.ID, zone.id); err != nil {
		log.Printf("saving timezone: %v", err)
		message = "Could not save timezone. Please try again."
	}
	if chosen.InlineMessageID == "" {
		return
	}
	keyboard := timezoneKeyboard()
	edit := tgbotapi.NewEditMessageText(0, 0, message)
	edit.InlineMessageID = chosen.InlineMessageID
	edit.ReplyMarkup = &keyboard
	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) { return bot.Request(edit) }); err != nil {
		log.Printf("editing timezone confirmation: %v", err)
	}
}
