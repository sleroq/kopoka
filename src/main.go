package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
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

func setupPixivClient() (*pixiv.AppPixivAPI, error) {
	tokenDeadlineSec, err := strconv.Atoi(os.Getenv("PIXIV_TOKEN_DEADLINE"))
	if err != nil {
		return nil, fmt.Errorf("parsing PIXIV_TOKEN_DEADLINE: %w", err)
	}

	tokenDeadline := time.Now().Add(time.Duration(tokenDeadlineSec) * time.Second)

	_, err = pixiv.LoadAuth(
		os.Getenv("PIXIV_ACCESS_TOKEN"),
		os.Getenv("PIXIV_REFRESH_TOKEN"),
		tokenDeadline,
	)

	if err != nil {
		return nil, fmt.Errorf("logging into Pixiv: %w", err)
	}

	app := pixiv.NewApp()

	return app, nil
}

func pixivCaption(illust *pixiv.Illust) string {
	return fmt.Sprintf(
		"<a href=\"%s\">%s</a>\n",
		"https://www.pixiv.net/artworks/"+strconv.Itoa(int(illust.ID)),
		illust.User.Name,
	)
}

func handlePixivLink(app *pixiv.AppPixivAPI, link *url.URL) ([]interface{}, error) {
	idStr := strings.Split(link.Path, "/artworks/")[1]
	id, err := strconv.Atoi(idStr)

	if err != nil {
		return nil, fmt.Errorf("parsing Pixiv ID: %w", err)
	}

	illust, err := app.IllustDetail(uint64(id))
	if err != nil {
		return nil, fmt.Errorf("fetching Pixiv illust: %w", err)
	}

	var photos []interface{}
	for i, page := range illust.MetaPages {
		file := tgbotapi.FileURL(page.Images.Original)
		photo := tgbotapi.NewInputMediaPhoto(file)

		if i%10 == 0 {
			photo.Caption = pixivCaption(illust)
			photo.ParseMode = "HTML"
		}

		photos = append(photos, photo)
	}

	if len(photos) == 0 {
		file := tgbotapi.FileURL(illust.MetaSinglePage.OriginalImageURL)
		photo := tgbotapi.NewInputMediaPhoto(file)
		photo.Caption = pixivCaption(illust)
		photo.ParseMode = "HTML"
		photos = append(photos, photo)
	}

	return photos, nil
}

func matchXImageLinks(body string, origPath string) ([]string, error) {
	re, err := regexp.Compile(`(?m)https://mosaic\.fxtwitter\.com/jpeg/\d+(/\w+)+`)
	if err != nil {
		return nil, fmt.Errorf("compiling regex: %w", err)
	}

	mosaicURL, err := url.Parse(re.FindString(body))
	if err != nil {
		return nil, fmt.Errorf("parsing mosaic URL: %w", err)
	}

	path := strings.Split(mosaicURL.Path, "/")
	IDsCount := len(path) - 3

	var links []string
	for i := 1; i <= IDsCount; i++ {
		imageUrl := fmt.Sprintf("https://d.fixupx.com%s/photo/%d", origPath, i)
		links = append(links, imageUrl)
	}

	if len(links) == 0 {
		re2, err := regexp.Compile(`(?m)<meta property="twitter:image" content="(.+?)"/>`)
		if err != nil {
			return nil, fmt.Errorf("compiling regex: %w", err)
		}

		matches := re2.FindStringSubmatch(body)
		if len(matches) == 2 {
			links = append(links, matches[1])
		}
	}

	return links, nil
}

func matchXAuthor(body string) (string, error) {
	re, err := regexp.Compile(`(?m)type="application/json\+oembed" title="(.*)"`)
	if err != nil {
		return "", fmt.Errorf("compiling regex: %w", err)
	}

	matches := re.FindStringSubmatch(body)

	if len(matches) < 2 {
		return "", fmt.Errorf("no matches found")
	}

	return matches[1], nil
}

func handleXLink(link *url.URL) ([]interface{}, error) {
	client := &http.Client{}
	client.Timeout = 10 * time.Second

	link.Host = "i.fixupx.com"
	res, err := client.Get(link.String())
	if err != nil {
		return nil, fmt.Errorf("fetching X link: %w", err)
	}
	defer res.Body.Close()
	link.Host = "fxtwitter.com"

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading X link response: %w", err)
	}

	author, err := matchXAuthor(string(body))
	if err != nil {
		return nil, fmt.Errorf("matching author name: %w", err)
	}

	links, err := matchXImageLinks(string(body), link.Path)
	if err != nil {
		return nil, fmt.Errorf("matching images: %w", err)
	}

	var photos []interface{}
	for i, imageLink := range links {
		file := tgbotapi.FileURL(imageLink)
		photo := tgbotapi.NewInputMediaPhoto(file)
		if i%10 == 0 {
			photo.Caption = fmt.Sprintf(
				"<a href=\"%s\">%s</a>\n",
				link.String(),
				author,
			)
			photo.ParseMode = "HTML"
		}
		photos = append(photos, photo)
	}

	return photos, nil
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

				var photos []interface{}

				switch parsed.Host {
				case "www.pixiv.net":
					fallthrough
				case "pixiv.net":
					photos, err = handlePixivLink(pApp, parsed)
				case "x.com":
					fallthrough
				case "fixupx.com":
					fallthrough
				case "vxtwitter.com":
					fallthrough
				case "fixvx.com":
					fallthrough
				case "twitter.com":
					fallthrough
				case "fxtwitter.com":
					photos, err = handleXLink(parsed)
				default:
					continue
				}

				if err != nil {
					log.Println(err)
					continue
				}

				photos = mergeArtPhotos(photos)
				err = sendPhotos(bot, update.Message.Chat.ID, update.Message.MessageID, photos)
				if err != nil {
					log.Println(err)
				}
			}
		}
	}
}
