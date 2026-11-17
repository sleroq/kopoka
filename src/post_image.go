package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const postImageVersion = 11
const defaultPostImageTheme = "dark"

type postImage struct {
	URL string `json:"url"`
}
type postVideo struct {
	ThumbnailURL string `json:"thumbnail_url"`
}
type postMedia struct {
	Photos   []postImage `json:"photos,omitempty"`
	Videos   []postVideo `json:"videos,omitempty"`
	Mosaic   *postImage  `json:"mosaic,omitempty"`
	Carousel bool        `json:"carousel,omitempty"`
	Bars     bool        `json:"bars,omitempty"`
}
type postArticle struct {
	Title       string     `json:"title"`
	PreviewText string     `json:"preview_text"`
	CoverMedia  *postImage `json:"cover_media,omitempty"`
}
type postCard struct {
	Title  string     `json:"title"`
	Domain string     `json:"domain"`
	Image  *postImage `json:"image,omitempty"`
}
type postPoll struct {
	Choices []struct {
		Label      string  `json:"label"`
		Percentage float64 `json:"percentage"`
	} `json:"choices"`
	TotalVotes uint64 `json:"total_votes"`
	TimeLeftEN string `json:"time_left_en"`
}

type renderPost struct {
	AuthorName       string       `json:"author_name"`
	AuthorUsername   string       `json:"author_username"`
	AuthorAvatarURL  string       `json:"author_avatar_url"`
	Text             string       `json:"text"`
	CreatedTimestamp int64        `json:"-"`
	CreatedAt        string       `json:"created_at,omitempty"`
	Likes            uint64       `json:"likes,omitempty"`
	Retweets         uint64       `json:"retweets,omitempty"`
	Replies          uint64       `json:"replies,omitempty"`
	Media            *postMedia   `json:"media,omitempty"`
	Quote            *renderPost  `json:"quote,omitempty"`
	Article          *postArticle `json:"article,omitempty"`
	Card             *postCard    `json:"card,omitempty"`
	Poll             *postPoll    `json:"poll,omitempty"`
}
type renderRequest struct {
	Theme      string            `json:"theme"`
	Post       renderPost        `json:"post"`
	ReplyChain []renderPost      `json:"reply_chain,omitempty"`
	HasMore    bool              `json:"has_more_in_chain,omitempty"`
	Images     map[string][]byte `json:"images,omitempty"`
}

type fxVideoFormat struct {
	URL       string `json:"url"`
	Container string `json:"container"`
	Bitrate   int    `json:"bitrate"`
}

type fxImage struct {
	URL          string          `json:"url"`
	ThumbnailURL string          `json:"thumbnail_url"`
	Type         string          `json:"type"`
	Formats      []fxVideoFormat `json:"formats"`
}
type fxRichPost struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Text     string `json:"text"`
	Created  int64  `json:"created_timestamp"`
	Likes    uint64 `json:"likes"`
	Retweets uint64 `json:"retweets"`
	Replies  uint64 `json:"replies"`
	Author   struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
		AvatarURL  string `json:"avatar_url"`
	} `json:"author"`
	ReplyingToStatus string       `json:"replying_to_status"`
	Media            *fxRichMedia `json:"media"`
	Quote            *fxRichPost  `json:"quote"`
	Article          *struct {
		Title       string `json:"title"`
		PreviewText string `json:"preview_text"`
		CoverMedia  *struct {
			MediaInfo struct {
				URL string `json:"original_img_url"`
			} `json:"media_info"`
		} `json:"cover_media"`
	} `json:"article"`
	Card *fxLinkCard `json:"card"`
	Poll *postPoll   `json:"poll"`
}

type fxRichMedia struct {
	Photos []fxImage `json:"photos"`
	Videos []fxImage `json:"videos"`
	All    []fxImage `json:"all"`
	Mosaic *struct {
		Formats struct {
			JPEG string `json:"jpeg"`
		} `json:"formats"`
	} `json:"mosaic"`
}

type fxLinkCard struct {
	URL    string   `json:"url"`
	Title  string   `json:"title"`
	Domain string   `json:"domain"`
	Image  *fxImage `json:"image"`
}

func (i fxImage) image() postImage { return postImage{URL: i.URL} }

func (p fxRichPost) normalize() (renderPost, error) {
	if p.Author.Name == "" || p.Author.ScreenName == "" {
		return renderPost{}, fmt.Errorf("FxTwitter response missing author")
	}
	out := renderPost{
		AuthorName: p.Author.Name, AuthorUsername: p.Author.ScreenName,
		AuthorAvatarURL: p.Author.AvatarURL, Text: p.Text,
		CreatedTimestamp: p.Created, Likes: p.Likes, Retweets: p.Retweets, Replies: p.Replies,
		Media: p.Media.normalize(), Poll: p.Poll.forDisplay(),
	}
	if p.Quote != nil {
		q, err := p.Quote.normalize()
		if err != nil {
			return out, err
		}
		out.Quote = &q
	}
	if p.Article != nil {
		out.Article = &postArticle{Title: p.Article.Title, PreviewText: p.Article.PreviewText}
		if p.Article.CoverMedia != nil {
			i := p.Article.CoverMedia.MediaInfo
			out.Article.CoverMedia = &postImage{URL: i.URL}
		}
	}
	if out.Media == nil && out.Article == nil {
		out.Card = p.Card.normalize()
		if out.Card != nil && p.Card.URL != "" {
			out.Text = strings.TrimRight(out.Text, " \t\n\r")
			out.Text = strings.TrimRight(strings.TrimSuffix(out.Text, p.Card.URL), " \t\n\r")
		}
	}
	return out, nil
}

func (p *postPoll) forDisplay() *postPoll {
	if p == nil {
		return nil
	}
	poll := *p
	if poll.TimeLeftEN != "" && poll.TimeLeftEN != "Final results" && !strings.HasSuffix(poll.TimeLeftEN, " left") {
		poll.TimeLeftEN += " left"
	}
	return &poll
}

func (m *fxRichMedia) normalize() *postMedia {
	if m == nil {
		return nil
	}
	out := &postMedia{}
	for _, image := range m.Photos {
		out.Photos = append(out.Photos, image.image())
	}
	for _, video := range m.Videos {
		out.Videos = append(out.Videos, postVideo{ThumbnailURL: video.ThumbnailURL})
	}
	if len(m.Photos) == 0 && len(m.Videos) == 0 {
		for _, image := range m.All {
			switch image.Type {
			case "photo", "":
				out.Photos = append(out.Photos, image.image())
			case "video", "gif":
				out.Videos = append(out.Videos, postVideo{ThumbnailURL: image.ThumbnailURL})
			}
		}
	}
	if len(out.Photos) == 0 && m.Mosaic != nil && m.Mosaic.Formats.JPEG != "" {
		out.Mosaic = &postImage{URL: m.Mosaic.Formats.JPEG}
	}
	if out.Mosaic == nil && len(out.Photos) == 0 && len(out.Videos) == 0 {
		return nil
	}
	return out
}

func (c *fxLinkCard) normalize() *postCard {
	if c == nil || c.Title == "" {
		return nil
	}
	out := &postCard{Title: c.Title, Domain: c.Domain}
	if out.Domain == "" {
		if link, err := url.Parse(c.URL); err == nil {
			out.Domain = link.Hostname()
		}
	}
	if c.Image != nil && c.Image.URL != "" {
		image := c.Image.image()
		out.Image = &image
	}
	return out
}

func fetchRichPost(client *http.Client, link *url.URL) (*fxRichPost, error) {
	var response struct {
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Tweet   *fxRichPost `json:"tweet"`
	}
	if err := fetchXResponse(client, link, "api.fxtwitter.com", &response); err != nil {
		return nil, err
	}
	if response.Code == http.StatusNotFound {
		return nil, errXPostNotFound
	}
	if response.Code != http.StatusOK || response.Tweet == nil {
		return nil, fmt.Errorf("FxTwitter API error %d: %s", response.Code, response.Message)
	}
	return response.Tweet, nil
}
func preparePostImage(link *url.URL, theme, zone string) (renderRequest, error) {
	return preparePostImageWithClient(&http.Client{Timeout: 10 * time.Second}, link, theme, zone)
}

func preparePostImageWithClient(client *http.Client, link *url.URL, theme, zone string) (renderRequest, error) {
	request, bars, err := preparePostImageVariants(client, link, zone, !strings.HasSuffix(theme, "-bars"))
	if strings.HasSuffix(theme, "-bars") {
		request = bars
	}
	request.Theme = strings.TrimSuffix(theme, "-bars")
	return request, err
}

func preparePostImageVariants(client *http.Client, link *url.URL, zone string, mergeMain bool) (renderRequest, renderRequest, error) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return renderRequest{}, renderRequest{}, err
	}

	source, err := fetchRichPost(client, xPostLink(link))
	if err != nil {
		return renderRequest{}, renderRequest{}, err
	}
	post, err := source.normalize()
	if err != nil {
		return renderRequest{}, renderRequest{}, err
	}
	chain, more, err := source.replyChain(client)
	if err != nil {
		return renderRequest{}, renderRequest{}, err
	}
	post = post.inTimezone(location)
	for i := range chain {
		chain[i] = chain[i].inTimezone(location)
	}
	request := renderRequest{Post: post, ReplyChain: chain, HasMore: more, Images: map[string][]byte{}}
	bars := request
	if request.Post.Media != nil {
		media := *request.Post.Media
		if len(media.Photos) > 1 {
			media.Carousel = true
			media.Bars = true
		}
		bars.Post.Media = &media
	}
	if err := loadPostAssets(client, request.Post, request.Images, mergeMain); err != nil {
		return request, bars, err
	}
	for _, p := range request.ReplyChain {
		if err := loadPostAssets(client, p, request.Images, true); err != nil {
			return request, bars, err
		}
	}
	return request, bars, nil
}

func (p renderPost) inTimezone(location *time.Location) renderPost {
	if p.CreatedTimestamp != 0 {
		p.CreatedAt = time.Unix(p.CreatedTimestamp, 0).In(location).Format("3:04 PM · Jan 2, 2006")
	}
	if p.Quote != nil {
		quote := p.Quote.inTimezone(location)
		p.Quote = &quote
	}
	return p
}

func (p *fxRichPost) replyChain(client *http.Client) ([]renderPost, bool, error) {
	var chain []renderPost
	for len(chain) < 3 && p.ReplyingToStatus != "" {
		id, err := strconv.ParseUint(p.ReplyingToStatus, 10, 64)
		if err != nil || id == 0 {
			return nil, false, fmt.Errorf("invalid FxTwitter reply parent")
		}
		link := &url.URL{Scheme: "https", Host: "x.com", Path: "/i/status/" + strconv.FormatUint(id, 10)}
		p, err = fetchRichPost(client, link)
		if err != nil {
			return nil, false, err
		}
		post, err := p.normalize()
		if err != nil {
			return nil, false, err
		}
		chain = append(chain, post)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, p.ReplyingToStatus != "", nil
}

func (p renderPost) imageURLs() []string {
	var urls []string

	if p.AuthorAvatarURL != "" {
		urls = append(urls, p.AuthorAvatarURL)
	}
	if p.Media != nil {
		if p.Media.Mosaic != nil {
			urls = append(urls, p.Media.Mosaic.URL)
		} else {
			for _, i := range p.Media.Photos {
				urls = append(urls, i.URL)
			}
		}
		for _, v := range p.Media.Videos {
			urls = append(urls, v.ThumbnailURL)
		}
	}
	if p.Article != nil && p.Article.CoverMedia != nil {
		urls = append(urls, p.Article.CoverMedia.URL)
	}
	if p.Card != nil && p.Card.Image != nil {
		urls = append(urls, p.Card.Image.URL)
	}
	return urls
}

func loadPostAssets(client *http.Client, p renderPost, images map[string][]byte, mergePhotos bool) error {
	for _, u := range p.imageURLs() {
		if u == "" {
			return fmt.Errorf("FxTwitter response missing image URL")
		}
		if _, ok := images[u]; ok {
			continue
		}
		res, err := client.Get(u)
		if err != nil {
			return fmt.Errorf("fetching image: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, 10_000_001))
		_ = res.Body.Close()
		if readErr != nil {
			return readErr
		}
		if res.StatusCode != http.StatusOK || len(data) == 0 || len(data) > 10_000_000 {
			return fmt.Errorf("fetching image %s: HTTP %s, %d bytes", u, res.Status, len(data))
		}
		images[u] = data
	}
	if p.Media != nil && mergePhotos {
		if err := p.Media.preparePhotos(images); err != nil {
			return err
		}
	}
	if p.Quote != nil {
		return loadPostAssets(client, *p.Quote, images, true)
	}
	return nil
}

func renderPostImage(request renderRequest) ([]byte, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	name := os.Getenv("KOPOKA_RENDERER")
	if name == "" {
		name = "kopoka-render"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name)
	cmd.Stdin = bytes.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rendering post image: %w: %s", err, stderr.String())
	}
	data := out.Bytes()
	if len(data) > 10_000_000 {
		return nil, fmt.Errorf("rendered JPEG exceeds Telegram size limit")
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid rendered JPEG: %w", err)
	}
	width, height := config.Width, config.Height
	if width <= 0 || height <= 0 || width+height > 10000 || max(width, height) > 20*min(width, height) {
		return nil, fmt.Errorf("invalid rendered JPEG dimensions %dx%d", width, height)
	}
	return data, nil
}
func postImageID(link *url.URL, theme string) string {
	return "post-image:" + theme + ":" + strings.TrimPrefix(artworkID(xPostLink(link)), "x:")
}
func postImageTheme(id string, link *url.URL) string {
	for _, option := range postImageThemeCatalog {
		for _, theme := range []string{option.ID, option.ID + "-bars"} {
			if id == postImageID(link, theme) || id == postImageID(link, theme)+":cached" {
				return theme
			}
		}
	}
	return ""
}
func findPostImage(db *sql.DB, id, theme, zone string) (string, string, error) {
	var file, caption string
	row := db.QueryRow(`SELECT file_id,caption FROM post_image_cache WHERE artwork_id=? AND theme=? AND timezone=? AND render_version=?`,
		id, theme, zone, postImageVersion)
	err := row.Scan(&file, &caption)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return file, caption, err
}
func inlinePostImageResults(db *sql.DB, user int64, link *url.URL, previews *thumbnails, instant bool) []any {
	zone, err := loadUserTimezone(db, user)
	if err != nil {
		log.Printf("loading post image timezone: %v", err)
		return nil
	}
	themes, err := loadPostImageThemes(db, user)
	if err != nil {
		log.Printf("loading post image themes: %v", err)
		return nil
	}

	var work *postImagePreviewWork
	var barsAvailable chan bool
	if !instant {
		work = previews.startPostImagePreview(link, zone, themes.Light, themes.Dark)
		barsAvailable = make(chan bool, 1)
		go func() { barsAvailable <- work.postImageHasBars() }()
	}
	imageThemes := []string{themes.Light, themes.Dark, themes.Light + "-bars", themes.Dark + "-bars"}
	var postImages []chan map[string]any
	for _, theme := range imageThemes {
		result := make(chan map[string]any, 1)
		postImages = append(postImages, result)
		go func() { result <- postImageResult(db, link, theme, zone, previews, work) }()
	}

	showBars := barsAvailable == nil || <-barsAvailable
	duplicateBars := previews.duplicatePostImageBars(link, zone, work)
	var results []any
	for i, result := range postImages {
		image := <-result
		if i < 2 || showBars && !duplicateBars {
			results = append(results, image)
		}
	}
	return results
}

func postImageResult(db *sql.DB, link *url.URL, theme, zone string, previews *thumbnails, work *postImagePreviewWork) map[string]any {
	file, caption, err := findPostImage(db, artworkID(xPostLink(link)), theme, zone)
	if err != nil {
		log.Printf("loading cached post image: %v", err)
	}
	var photo previewPhoto
	if file != "" {
		photo = previewPhoto{fileID: file}
	} else {
		photo = previews.postImagePreview(link, theme, zone, work)
	}
	result := inlinePhoto(photo)
	result["id"] = postImageID(link, theme)
	baseTheme := strings.TrimSuffix(theme, "-bars")
	result["title"] = "Post image · " + strings.ToUpper(baseTheme[:1]) + baseTheme[1:]
	if baseTheme != theme {
		result["title"] = result["title"].(string) + " · Bars"
	}
	if file != "" {
		result["id"] = postImageID(link, theme) + ":cached"
		result["caption"] = caption
		result["parse_mode"] = "HTML"
		return result
	}
	result["input_message_content"] = map[string]any{
		"message_text": "Loading post image…", "disable_web_page_preview": true,
		"entities": []any{map[string]any{"type": "text_link", "offset": 0, "length": 19, "url": link.String()}},
	}
	wait := map[string]any{"text": "Please wait…", "disabled": map[string]any{}}
	result["reply_markup"] = map[string]any{"inline_keyboard": []any{[]any{wait}}}
	return result
}
func postImagePhoto(db *sql.DB, link *url.URL, theme, zone string, previews *thumbnails) (tgbotapi.InputMediaPhoto, error) {
	file, caption, err := findPostImage(db, artworkID(xPostLink(link)), theme, zone)
	if err != nil {
		return tgbotapi.InputMediaPhoto{}, err
	}
	if file != "" {
		photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileID(file))
		photo.Caption, photo.ParseMode = caption, "HTML"
		return photo, nil
	}
	if previews != nil {
		if variant := previews.existingPostImagePreview(link, theme, zone); variant != nil {
			photo, err := variant.photo()
			if err == nil {
				return photo, nil
			}
			log.Printf("reusing post image preview for %s (%s): %v", link, theme, err)
		}
	}
	request, err := preparePostImage(link, theme, zone)
	if err != nil {
		return tgbotapi.InputMediaPhoto{}, err
	}
	data, err := renderPostImage(request)
	if err != nil {
		return tgbotapi.InputMediaPhoto{}, err
	}
	photo := tgbotapi.NewInputMediaPhoto(tgbotapi.FileBytes{Name: "post.jpg", Bytes: data})
	photo.Caption = postImageCaption(link, request)
	photo.ParseMode = "HTML"
	return photo, nil
}

func postImageCaption(link *url.URL, request renderRequest) string {
	return fmt.Sprintf(`<a href="%s">%s</a>`,
		html.EscapeString(xPostLink(link).String()), html.EscapeString(request.Post.AuthorName))
}

func savePostImage(db *sql.DB, link *url.URL, theme, zone, fileID, caption string) error {
	_, err := db.Exec(`INSERT INTO post_image_cache(artwork_id,theme,timezone,render_version,file_id,caption) VALUES(?,?,?,?,?,?)
 ON CONFLICT(artwork_id,theme,timezone,render_version) DO UPDATE SET file_id=excluded.file_id,caption=excluded.caption`,
		artworkID(xPostLink(link)), theme, zone, postImageVersion, fileID, caption)
	return err
}

func sendPostImage(bot *tgbotapi.BotAPI, db *sql.DB, message *tgbotapi.Message, link *url.URL, theme string) error {
	zone, err := loadUserTimezone(db, message.From.ID)
	if err != nil {
		return err
	}
	photo, err := postImagePhoto(db, link, theme, zone, nil)
	if err != nil {
		return err
	}
	ids, err := sendPhotosWithIDs(bot, message.Chat.ID, message.MessageID, []any{photo})
	if err != nil {
		return err
	}
	if len(ids) == 1 {
		if err := savePostImage(db, link, theme, zone, ids[0], photo.Caption); err != nil {
			log.Printf("saving chat post image: %v", err)
		}
	}
	return nil
}

func editInlinePostImage(bot *tgbotapi.BotAPI, db *sql.DB, chosen *tgbotapi.ChosenInlineResult, cacheChat string, link *url.URL, theme string, previews *thumbnails) error {
	zone, err := loadUserTimezone(db, chosen.From.ID)
	if err != nil {
		return err
	}
	photo, err := postImagePhoto(db, link, theme, zone, previews)
	if err != nil {
		return err
	}
	file, cached := photo.Media.(tgbotapi.FileID)
	if !cached {
		id, err := cacheInlineMedia(bot, cacheChat, photo)
		if err != nil {
			return err
		}
		if err := savePostImage(db, link, theme, zone, id, photo.Caption); err != nil {
			return err
		}
		file = tgbotapi.FileID(id)
	}
	payload, err := json.Marshal(map[string]any{
		"type": "photo", "media": file, "caption": photo.Caption, "parse_mode": photo.ParseMode,
	})
	if err != nil {
		return err
	}
	_, err = retryTelegram(func() (*tgbotapi.APIResponse, error) {
		params := tgbotapi.Params{
			"inline_message_id": chosen.InlineMessageID, "media": string(payload),
			"reply_markup": `{"inline_keyboard":[]}`,
		}
		return bot.MakeRequest("editMessageMedia", params)
	})
	if err != nil {
		return err
	}
	return selectArtwork(db, chosen.From.ID, artworkID(xPostLink(link)))
}
