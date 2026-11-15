package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const inlinePostTextID = "post:text"

func inlinePostTextResult(link *url.URL, previews *thumbnails) map[string]any {
	result := inlinePhoto(previews.postTextPhoto())
	result["id"], result["title"] = inlinePostTextID, "Post with text"
	result["description"] = "Post, quote and reply thread"
	result["input_message_content"] = map[string]any{
		"message_text": "Loading X post…", "disable_web_page_preview": true,
		"entities": []any{map[string]any{"type": "text_link", "offset": 0, "length": 15, "url": xPostLink(link).String()}},
	}
	result["reply_markup"] = map[string]any{"inline_keyboard": []any{[]any{
		map[string]any{"text": "Please wait…", "disabled": map[string]any{}},
	}}}
	return result
}

func xPostAttachments(post *fxRichPost) ([]xMedia, error) {
	if post == nil || post.Media == nil {
		return nil, nil
	}
	images := post.Media.All
	if len(images) == 0 {
		images = append([]fxImage(nil), post.Media.Photos...)
		for _, video := range post.Media.Videos {
			if video.Type == "" {
				video.Type = "video"
			}
			images = append(images, video)
		}
	}
	media := make([]xMedia, 0, len(images))
	for _, image := range images {
		kind := image.Type
		switch kind {
		case "", "photo":
			kind = "photo"
		case "video", "gif":
			kind = "video"
		default:
			return nil, fmt.Errorf("unsupported FxTwitter media type %q", kind)
		}
		if image.URL == "" {
			return nil, fmt.Errorf("FxTwitter response missing media URL")
		}
		address := image.URL
		if kind == "photo" {
			address = xPhotoURL(address)
		}
		media = append(media, xMedia{Type: kind, URL: address})
	}
	return media, nil
}

type cachedPostMedia struct {
	kind, id string
}

func cachePostMedia(bot *tgbotapi.BotAPI, cacheChat string, media []xMedia) ([]cachedPostMedia, error) {
	cached := make([]cachedPostMedia, 0, len(media))
	for _, item := range media {
		var input any = tgbotapi.NewInputMediaPhoto(tgbotapi.FileURL(item.URL))
		if item.Type == "video" {
			input = tgbotapi.NewInputMediaVideo(tgbotapi.FileURL(item.URL))
		}
		id, err := cacheInlineMedia(bot, cacheChat, input)
		if err != nil {
			return nil, err
		}
		cached = append(cached, cachedPostMedia{kind: item.Type, id: id})
	}
	return cached, nil
}

func richPostMedia(markup *strings.Builder, media []cachedPostMedia, offset int) {
	if len(media) > 1 {
		markup.WriteString("<tg-slideshow>")
	}
	for i, item := range media {
		id := strconv.Itoa(offset + i)
		if item.kind == "video" {
			fmt.Fprintf(markup, `<video src="tg://video?id=%s"></video>`, id)
		} else {
			fmt.Fprintf(markup, `<img src="tg://photo?id=%s"/>`, id)
		}
	}
	if len(media) > 1 {
		markup.WriteString("</tg-slideshow>")
	}
}

func richPostBody(text string) string {
	return strings.ReplaceAll(html.EscapeString(text), "\n", "<br>")
}

func richPostAuthor(post *fxRichPost) string {
	author := "<b>" + xPostAuthorHTML(post) + "</b>"
	if post.Author.ScreenName != "" {
		author += " · @" + html.EscapeString(post.Author.ScreenName)
	}
	return author
}

func limitRichPostText(post *fxRichPost, parents []*fxRichPost) {
	posts := []*fxRichPost{post}
	if post.Quote != nil {
		posts = append(posts, post.Quote)
	}
	for _, parent := range slices.Backward(parents) {
		posts = append(posts, parent)
	}
	remaining := 32768 - 64
	for _, p := range posts {
		remaining -= utf8.RuneCountInString(p.Author.Name) + utf8.RuneCountInString(p.Author.ScreenName) + 4
	}
	for _, p := range posts {
		runes := []rune(p.Text)
		if len(runes) > remaining {
			if remaining > 0 {
				p.Text = string(runes[:remaining-1]) + "…"
			} else {
				p.Text = ""
			}
			remaining = 0
		} else {
			remaining -= len(runes)
		}
	}
}

func richXPost(post *fxRichPost, parents []*fxRichPost, more bool, own, quote []cachedPostMedia) map[string]any {
	limitRichPostText(post, parents)
	var markup strings.Builder
	if len(parents) > 0 || more {
		markup.WriteString(`<p>Reply thread from `)
		if link := inlineLink(post.URL); link != nil {
			fmt.Fprintf(&markup, `<a href="%s">X</a>`, html.EscapeString(link.String()))
		} else {
			markup.WriteString("X")
		}
		markup.WriteString(":</p>")
		if more {
			markup.WriteString("<p>…</p>")
		}
		for _, parent := range parents {
			markup.WriteString("<blockquote expandable>")
			markup.WriteString(richPostAuthor(parent))
			if parent.Text != "" {
				markup.WriteString("<br>")
				markup.WriteString(richPostBody(parent.Text))
			}
			markup.WriteString("</blockquote>")
		}
	}
	markup.WriteString("<p>")
	markup.WriteString(richPostAuthor(post))
	markup.WriteString(" on X</p>")
	if post.Text != "" {
		markup.WriteString("<p>")
		markup.WriteString(richPostBody(post.Text))
		markup.WriteString("</p>")
	}
	richPostMedia(&markup, own, 0)
	if post.Quote != nil {
		markup.WriteString("<blockquote><p>")
		markup.WriteString(richPostAuthor(post.Quote))
		markup.WriteString("</p>")
		if post.Quote.Text != "" {
			markup.WriteString("<p>")
			markup.WriteString(richPostBody(post.Quote.Text))
			markup.WriteString("</p>")
		}
		richPostMedia(&markup, quote, len(own))
		markup.WriteString("</blockquote>")
	}

	media := make([]any, 0, len(own)+len(quote))
	for i, item := range slices.Concat(own, quote) {
		media = append(media, map[string]any{
			"id": strconv.Itoa(i), "media": map[string]any{"type": item.kind, "media": item.id},
		})
	}
	result := map[string]any{"html": markup.String()}
	if len(media) > 0 {
		result["media"] = media
	}
	return result
}

func editInlinePostText(bot *tgbotapi.BotAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, cacheChat string, link *url.URL) error {
	post, parents, more, err := loadXPostThread(&http.Client{Timeout: 10 * time.Second}, link)
	if err != nil {
		return err
	}

	own, err := xPostAttachments(post)
	if err != nil {
		return err
	}
	quote, err := xPostAttachments(post.Quote)
	if err != nil {
		return err
	}
	attachments := slices.Concat(own, quote)
	if len(attachments) > 50 {
		return fmt.Errorf("post exceeds Telegram's 50 attachment limit")
	}

	rich, err := xPostRichTextEnabled(db, chosen.From.ID)
	if err != nil {
		return err
	}

	params := tgbotapi.Params{"inline_message_id": chosen.InlineMessageID, "reply_markup": `{"inline_keyboard":[]}`}
	method := "editMessageText"
	if rich {
		cached, err := cachePostMedia(bot, cacheChat, attachments)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(richXPost(post, parents, more, cached[:len(own)], cached[len(own):]))
		if err != nil {
			return err
		}
		params["rich_message"] = string(payload)
	} else {
		text := normalXPost(post, parents, more, len(attachments) > 0)
		if len(attachments) > 0 {
			cached, err := cachePostMedia(bot, cacheChat, attachments[:1])
			if err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]any{
				"type": cached[0].kind, "media": cached[0].id, "caption": text,
				"parse_mode": "HTML", "show_caption_above_media": true,
			})
			if err != nil {
				return err
			}
			method, params["media"] = "editMessageMedia", string(payload)
		} else {
			params["text"], params["parse_mode"], params["disable_web_page_preview"] = text, "HTML", "true"
		}
	}

	if _, err := retryTelegram(func() (*tgbotapi.APIResponse, error) { return bot.MakeRequest(method, params) }); err != nil {
		return fmt.Errorf("editing inline post: %w", err)
	}
	return selectArtwork(db, chosen.From.ID, artworkID(xPostLink(link)))
}
