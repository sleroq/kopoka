package main

import (
	"database/sql"
	"fmt"
	"html"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func setupTelegramBot() (*tgbotapi.BotAPI, *tgbotapi.UpdateConfig, error) {
	bot, err := tgbotapi.NewBotAPI(os.Getenv("BOT_TOKEN"))
	if err != nil {
		return nil, nil, fmt.Errorf("creating telegram bot api instance: %w", err)
	}

	if os.Getenv("DEBUG") == "true" {
		bot.Debug = true

		log.Println("Debug mode enabled")
	}

	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "channel_post", "inline_query", "chosen_inline_result", "callback_query"}

	return bot, &u, nil
}

func pixivCaption(illust *pixiv.Illust) string {
	return fmt.Sprintf(
		"<a href=\"%s\">%s</a>",
		"https://www.pixiv.net/artworks/"+strconv.Itoa(int(illust.ID)),
		html.EscapeString(illust.User.Name),
	)
}

// The Pixiv client refreshes package-level credentials without synchronization.
var pixivRequestMu sync.Mutex

func handlePixivLink(app *pixiv.AppPixivAPI, link *url.URL, metadata *artwork) ([]any, error) {
	idStr, ok := strings.CutPrefix(link.Path, "/artworks/")
	if !ok {
		return nil, fmt.Errorf("invalid Pixiv artwork path: %s", link.Path)
	}
	id, err := strconv.Atoi(idStr)

	if err != nil {
		return nil, fmt.Errorf("parsing Pixiv ID: %w", err)
	}

	pixivRequestMu.Lock()
	illust, err := app.IllustDetail(uint64(id))
	pixivRequestMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("fetching Pixiv illust: %w", err)
	}

	metadata.Author, metadata.Text = illust.User.Name, illust.Title
	client := &http.Client{Timeout: 30 * time.Second}
	if illust.Type == "ugoira" {
		pixivRequestMu.Lock()
		ugoira, err := app.UgoiraMetadata(uint64(id))
		pixivRequestMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("fetching Pixiv ugoira metadata: %w", err)
		}
		file, err := downloadPixivUgoira(client, &ugoira.UgoiraMetadataUgoiraMetadata)
		if err != nil {
			return nil, err
		}
		animation := tgbotapi.NewInputMediaAnimation(file)
		animation.Caption = pixivCaption(illust)
		animation.ParseMode = "HTML"
		return []any{animation}, nil
	}
	var photos []any
	for i, page := range illust.MetaPages {
		file, err := downloadPixivPhoto(client, page.Images.Large, page.Images.Original)
		if err != nil {
			return nil, err
		}
		photo := tgbotapi.NewInputMediaPhoto(file)

		if i%10 == 0 {
			photo.Caption = pixivCaption(illust)
			photo.ParseMode = "HTML"
		}

		photos = append(photos, photo)
	}

	if len(photos) == 0 {
		file, err := downloadPixivPhoto(client, illust.Images.Large, illust.MetaSinglePage.OriginalImageURL)
		if err != nil {
			return nil, err
		}
		photo := tgbotapi.NewInputMediaPhoto(file)
		photo.Caption = pixivCaption(illust)
		photo.ParseMode = "HTML"
		photos = append(photos, photo)
	}

	return photos, nil
}

func handleXLink(link *url.URL, metadata *artwork) ([]any, error) {
	post, err := fetchXPost(link, false)
	if err != nil {
		return nil, err
	}

	metadata.Author, metadata.Text = post.Author, post.Text
	link.Host = "fxtwitter.com"
	var photos []any
	for i, image := range post.Media {
		var base tgbotapi.BaseInputMedia
		switch image.Type {
		case "", "photo":
			base = tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(xPhotoURL(image.URL))).BaseInputMedia
		case "video", "gif":
			base = tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(image.URL)).BaseInputMedia
		default:
			return nil, fmt.Errorf("unsupported FxTwitter media type %q", image.Type)
		}
		if i%10 == 0 {
			base.Caption = fmt.Sprintf(
				"<a href=\"%s\">%s</a>",
				html.EscapeString(link.String()),
				html.EscapeString(post.Author),
			)
			base.ParseMode = "HTML"
		}
		if base.Type == "video" {
			photos = append(photos, tgbotapi.InputMediaVideo{BaseInputMedia: base})
		} else {
			photos = append(photos, tgbotapi.InputMediaPhoto{BaseInputMedia: base})
		}
	}
	return photos, nil
}

func showProcessing(bot *tgbotapi.BotAPI, chatID int64, action string) func() {
	sendAction := func() {
		if _, err := bot.Request(tgbotapi.NewChatAction(chatID, action)); err != nil {
			log.Printf("sending chat action: %v", err)
		}
	}
	sendAction()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sendAction()
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func supportedLink(link *url.URL) bool {
	switch link.Host {
	case "www.pixiv.net", "pixiv.net", "x.com", "fixupx.com", "vxtwitter.com", "fixvx.com", "twitter.com", "fxtwitter.com":
		return true
	}
	return false
}

func fetchArtPhotos(app *pixiv.AppPixivAPI, link *url.URL, metadata *artwork) ([]any, error) {
	var photos []any
	var err error
	switch link.Host {
	case "www.pixiv.net", "pixiv.net":
		photos, err = handlePixivLink(app, link, metadata)
	default:
		photos, err = handleXLink(link, metadata)
	}
	if err != nil {
		return nil, err
	}
	return mergeArtPhotos(photos), nil
}

func processLink(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, message *tgbotapi.Message, link *url.URL, previews *thumbnails) error {
	if !supportedLink(link) {
		return nil
	}

	stop := showProcessing(bot, message.Chat.ID, tgbotapi.ChatTyping)
	valid := inlineLink(link.String())
	a := artwork{}
	if valid != nil {
		link = valid
		a.ID, a.URL = artworkID(valid), valid.String()
	}
	photos, err := fetchArtPhotos(app, link, &a)
	stop()
	if err != nil {
		return err
	}
	if len(photos) == 0 {
		return fmt.Errorf("no media found")
	}

	action := tgbotapi.ChatUploadPhoto
	for _, item := range photos {
		if mediaBase(item).Type == "video" || mediaBase(item).Type == "animation" {
			action = tgbotapi.ChatUploadVideo
			break
		}
	}
	stop = showProcessing(bot, message.Chat.ID, action)
	defer stop()
	ids, err := sendPhotosWithIDs(bot, message.Chat.ID, message.MessageID, photos)
	if err != nil {
		return err
	}

	if valid != nil && len(photos) > 0 && len(ids) == len(photos) {
		a.Photos = ids
		a.Caption = mediaBase(photos[0]).Caption
		for _, item := range photos {
			a.Types = append(a.Types, mediaBase(item).Type)
		}
		if err := saveArtwork(db, message.From.ID, a, false); err != nil {
			log.Printf("saving chat artwork: %v", err)
		} else if previews != nil {
			previews.warmArtwork(inlineLink(a.URL), len(a.Photos))
		}
	}
	return nil
}

var codeURLPattern = regexp.MustCompile(`https?://[^\s<>"\x60]+`)

func entityText(text []uint16, entity tgbotapi.MessageEntity) string {
	start, length := entity.Offset, entity.Length
	if start < 0 || length <= 0 || start > len(text) || length > len(text)-start {
		return ""
	}
	end := start + length
	if start > 0 && start < len(text) && text[start] >= 0xDC00 && text[start] <= 0xDFFF ||
		end < len(text) && text[end] >= 0xDC00 && text[end] <= 0xDFFF {
		return ""
	}
	return string(utf16.Decode(text[start:end]))
}

func messageLinks(message *tgbotapi.Message) []*url.URL {
	text := utf16.Encode([]rune(message.Text))
	seen := make(map[string]bool)
	var links []*url.URL
	for _, entity := range message.Entities {
		var candidates []string
		switch entity.Type {
		case "text_link":
			candidates = []string{entity.URL}
		case "url":
			candidates = []string{entityText(text, entity)}
		case "code", "pre":
			for _, link := range codeURLPattern.FindAllString(entityText(text, entity), -1) {
				candidates = append(candidates, strings.TrimRight(link, ".,;:!?)]}"))
			}
		}
		for _, candidate := range candidates {
			link, err := url.Parse(candidate)
			if err != nil || !supportedLink(link) {
				continue
			}
			link.RawQuery = ""
			if !seen[link.String()] {
				seen[link.String()] = true
				links = append(links, link)
			}
		}
	}
	return links
}

func handleMessage(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, message *tgbotapi.Message, queue *mediaQueue, previews *thumbnails) {
	command := message.CommandWithAt()
	if command == "help" || command == "start" || strings.EqualFold(command, "help@"+bot.Self.UserName) || strings.EqualFold(command, "start@"+bot.Self.UserName) {
		reply := tgbotapi.NewMessage(message.Chat.ID, "Just send me x.com / pixiv.net link with media in it. Or use me inline")
		reply.ReplyToMessageID = message.MessageID
		query := ""
		reply.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.InlineKeyboardButton{Text: "Open inline", SwitchInlineQueryCurrentChat: &query}))
		if _, err := bot.Send(reply); err != nil {
			log.Printf("sending help: %v", err)
		}
		return
	}
	if command == "json" || strings.EqualFold(command, "json@"+bot.Self.UserName) {
		if err := sendChatJSON(bot, message); err != nil {
			log.Printf("sending chat JSON: %v", err)
		}
		return
	}

	if message.From == nil {
		return
	}
	log.Printf("[%s] %s", message.From.UserName, message.Text)
	if command == "settings" || strings.EqualFold(command, "settings@"+bot.Self.UserName) {
		showSettings(bot, db, message)
		return
	}
	for _, name := range []string{"historyoff", "historyon"} {
		if command == name || strings.EqualFold(command, name+"@"+bot.Self.UserName) {
			enabled := name == "historyon"
			text := historyOffText
			if enabled {
				text = "History is on. New selections will be saved."
			}
			if err := setHistoryEnabled(db, message.From.ID, enabled); err != nil {
				log.Printf("changing history preference: %v", err)
				text = "Could not change your history preference. Please try again."
			}
			reply := tgbotapi.NewMessage(message.Chat.ID, text)
			reply.ReplyToMessageID = message.MessageID
			if !enabled && text == historyOffText {
				count, err := historyCount(db, message.From.ID)
				if err != nil {
					log.Printf("counting history: %v", err)
				} else if count > 0 {
					reply.ReplyMarkup = historyDeleteKeyboard(message.From.ID)
				}
			}
			if _, err := bot.Send(reply); err != nil {
				log.Printf("sending history preference reply: %v", err)
			}
			return
		}
	}
	processMessageLinks(bot, app, db, message, queue, previews)
}

func reactToMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message) {
	reactions := []string{"❤", "🥰", "🍓"}
	params := tgbotapi.Params{
		"chat_id":    strconv.FormatInt(message.Chat.ID, 10),
		"message_id": strconv.Itoa(message.MessageID),
		"reaction":   fmt.Sprintf(`[{"type":"emoji","emoji":%q}]`, reactions[rand.IntN(len(reactions))]),
	}
	if _, err := bot.MakeRequest("setMessageReaction", params); err != nil {
		log.Printf("reacting to message: %v", err)
	}
}

func processMessageLinks(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, db *sql.DB, message *tgbotapi.Message, queue *mediaQueue, previews *thumbnails) {
	links := messageLinks(message)
	if len(links) > 0 {
		reactToMessage(bot, message)
	}

	type linkResult struct {
		link   string
		result <-chan error
	}
	var results []linkResult
	rejected := make(map[error]int)
	for _, link := range links {
		linkText := link.String()
		result, err := queue.submit(message.From.ID, func() error {
			return processLink(bot, app, db, message, link, previews)
		})
		if err != nil {
			rejected[err]++
		} else {
			results = append(results, linkResult{linkText, result})
		}
	}
	for _, reason := range []error{errMediaQueueFull, errMediaHourlyLimit, errMediaDailyLimit} {
		if count := rejected[reason]; count > 0 {
			text := fmt.Sprintf("Links not queued: %d (%s). Please retry later.", count, reason)
			reply := tgbotapi.NewMessage(message.Chat.ID, text)
			reply.ReplyToMessageID = message.MessageID
			if _, err := bot.Send(reply); err != nil {
				log.Printf("sending media rejection: %v", err)
			}
		}
	}
	for _, result := range results {
		if err := <-result.result; err != nil {
			log.Printf("processing %s: %v", result.link, err)
			reply := tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("Could not process %s. Please try again.", result.link))
			reply.ReplyToMessageID = message.MessageID
			if _, err := bot.Send(reply); err != nil {
				log.Printf("sending processing error: %v", err)
			}
		}
	}
}

func main() {
	limits, err := mediaLimitsFromEnv()
	if err != nil {
		log.Fatalf("invalid media configuration: %v", err)
	}
	queue := newMediaQueue(limits)
	path := os.Getenv("SQLITE_PATH")
	if path == "" {
		path = "kopoka.db"
	}
	db, err := openArtworkDB(path)
	if err != nil {
		log.Panicf("opening artwork database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("closing artwork database: %v", err)
		}
	}()
	bot, u, err := setupTelegramBot()
	if err != nil {
		log.Panic(err)
	}

	pApp, err := setupPixivClient()
	if err != nil {
		log.Panic(err)
	}

	previews, err := startThumbnails(pApp, bot, db, os.Getenv("INLINE_CACHE_CHAT_ID"))
	if err != nil {
		log.Panicf("starting thumbnails: %v", err)
	}

	go previews.backfillHistory()

	updates := telegramUpdates(bot, *u)

	for update := range updates {
		if update.InlineQuery != nil {
			go handleInlineQuery(bot, db, update.InlineQuery, previews)
		}
		if update.ChosenInlineResult != nil {
			cacheChat := os.Getenv("INLINE_CACHE_CHAT_ID")
			go handleChosenInlineResult(bot, pApp, db, update.ChosenInlineResult, cacheChat, queue, previews)
		}
		if update.CallbackQuery != nil {
			go handleIncomingCallback(bot, pApp, db, update.CallbackQuery, queue)
		}
		if update.Message != nil {
			go handleIncomingMessage(bot, pApp, db, update.Message, queue, previews)
		}
		if update.ChannelPost != nil {
			go handleIncomingMessage(bot, pApp, db, update.ChannelPost, queue, previews)
		}
	}
}

func handleCallback(bot *tgbotapi.BotAPI, db *sql.DB, query *tgbotapi.CallbackQuery) {
	if strings.HasPrefix(query.Data, "settings:") {
		handleSettingsCallback(bot, db, query)
	} else if strings.HasPrefix(query.Data, "history:") {
		handleHistoryCallback(bot, db, query)
	}
}
