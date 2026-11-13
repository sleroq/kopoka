package main

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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

	return bot, &u, nil
}

func pixivCaption(illust *pixiv.Illust) string {
	return fmt.Sprintf(
		"<a href=\"%s\">%s</a>\n",
		"https://www.pixiv.net/artworks/"+strconv.Itoa(int(illust.ID)),
		illust.User.Name,
	)
}

func handlePixivLink(app *pixiv.AppPixivAPI, link *url.URL) ([]any, error) {
	idStr := strings.Split(link.Path, "/artworks/")[1]
	id, err := strconv.Atoi(idStr)

	if err != nil {
		return nil, fmt.Errorf("parsing Pixiv ID: %w", err)
	}

	illust, err := app.IllustDetail(uint64(id))
	if err != nil {
		return nil, fmt.Errorf("fetching Pixiv illust: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var photos []any
	for i, page := range illust.MetaPages {
		file, err := downloadPixivPhoto(client, page.Images.Original, page.Images.Large)
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
		file, err := downloadPixivPhoto(client, illust.MetaSinglePage.OriginalImageURL, illust.Images.Large)
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

func handleXLink(link *url.URL) ([]any, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	apiLink := *link
	apiLink.Host = "api.fxtwitter.com"
	res, err := client.Get(apiLink.String())
	if err != nil {
		return nil, fmt.Errorf("fetching X link: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching X link: HTTP %s", res.Status)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Tweet   *struct {
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Media struct {
				Photos []struct {
					URL string `json:"url"`
				} `json:"photos"`
			} `json:"media"`
		} `json:"tweet"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("decoding X link response: %w", err)
	}
	if response.Code != http.StatusOK || response.Tweet == nil {
		return nil, fmt.Errorf("FxTwitter API error %d: %s", response.Code, response.Message)
	}
	if response.Tweet.Author.Name == "" {
		return nil, fmt.Errorf("FxTwitter response missing author name")
	}

	link.Host = "fxtwitter.com"
	var photos []any
	for i, image := range response.Tweet.Media.Photos {
		if image.URL == "" {
			return nil, fmt.Errorf("FxTwitter response missing photo URL at index %d", i)
		}
		photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(image.URL))
		if i%10 == 0 {
			photo.Caption = fmt.Sprintf(
				"<a href=\"%s\">%s</a>\n",
				html.EscapeString(link.String()),
				html.EscapeString(response.Tweet.Author.Name),
			)
			photo.ParseMode = "HTML"
		}
		photos = append(photos, photo)
	}
	return photos, nil
}

func showProcessing(bot *tgbotapi.BotAPI, chatID int64) func() {
	sendAction := func() {
		if _, err := bot.Request(tgbotapi.NewChatAction(chatID, tgbotapi.ChatUploadPhoto)); err != nil {
			log.Printf("sending chat action: %v", err)
		}
	}
	sendAction()
	stop := make(chan struct{})
	go func() {
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
	return func() { close(stop) }
}

func processLink(bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, message *tgbotapi.Message, link *url.URL) error {
	var fetchPhotos func() ([]any, error)
	switch link.Host {
	case "www.pixiv.net", "pixiv.net":
		fetchPhotos = func() ([]any, error) { return handlePixivLink(app, link) }
	case "x.com", "fixupx.com", "vxtwitter.com", "fixvx.com", "twitter.com", "fxtwitter.com":
		fetchPhotos = func() ([]any, error) { return handleXLink(link) }
	default:
		return nil
	}

	stop := showProcessing(bot, message.Chat.ID)
	defer stop()

	photos, err := fetchPhotos()
	if err != nil {
		return err
	}
	return sendPhotos(bot, message.Chat.ID, message.MessageID, mergeArtPhotos(photos))
}

func main() {
	bot, u, err := setupTelegramBot()
	if err != nil {
		log.Panic(err)
	}

	pApp, err := setupPixivClient()
	if err != nil {
		log.Panic(err)
	}

	updates := bot.GetUpdatesChan(*u)

	for update := range updates {
		if update.Message != nil { // If we got a message
			log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

			var urls []string

			for _, entity := range update.Message.Entities {
				if entity.Type == "url" {
					urls = append(urls, update.Message.Text[entity.Offset:entity.Offset+entity.Length])
				}
			}

			for _, link := range urls {
				parsed, err := url.Parse(link)
				if err != nil {
					log.Println(err)
					continue
				}
				parsed.RawQuery = ""

				if err := processLink(bot, pApp, update.Message, parsed); err != nil {
					log.Println(err)
				}
			}
		}
	}
}
