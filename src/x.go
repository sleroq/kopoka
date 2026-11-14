package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type xMedia struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type xPost struct {
	Text   string
	Author string
	Media  []xMedia
}

func fetchXPost(link *url.URL) (xPost, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	post, fxErr := fetchFxPost(client, link)
	if fxErr == nil {
		return post, nil
	}
	post, vxErr := fetchVxPost(client, link)
	if vxErr == nil {
		return post, nil
	}
	return xPost{}, fmt.Errorf("FxTwitter: %w; VxTwitter: %w", fxErr, vxErr)
}

func fetchXResponse(client *http.Client, link *url.URL, host string, response any) error {
	apiLink := *link
	apiLink.Host = host
	res, err := client.Get(apiLink.String())
	if err != nil {
		return fmt.Errorf("fetching X link: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching X link: HTTP %s", res.Status)
	}
	if err := json.NewDecoder(res.Body).Decode(response); err != nil {
		return fmt.Errorf("decoding X link response: %w", err)
	}
	return nil
}

func fetchFxPost(client *http.Client, link *url.URL) (xPost, error) {
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Tweet   *struct {
			Text   string `json:"text"`
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Media struct {
				All    []xMedia `json:"all"`
				Photos []xMedia `json:"photos"`
			} `json:"media"`
		} `json:"tweet"`
	}
	if err := fetchXResponse(client, link, "api.fxtwitter.com", &response); err != nil {
		return xPost{}, err
	}
	if response.Code != http.StatusOK || response.Tweet == nil {
		return xPost{}, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}
	items := response.Tweet.Media.All
	if items == nil {
		items = response.Tweet.Media.Photos
	}
	post := xPost{Text: response.Tweet.Text, Author: response.Tweet.Author.Name, Media: items}
	if err := validateXPost(post, "FxTwitter"); err != nil {
		return xPost{}, err
	}
	return post, nil
}

func fetchVxPost(client *http.Client, link *url.URL) (xPost, error) {
	var response struct {
		Text     string   `json:"text"`
		UserName string   `json:"user_name"`
		Media    []xMedia `json:"media_extended"`
	}
	if err := fetchXResponse(client, link, "api.vxtwitter.com", &response); err != nil {
		return xPost{}, err
	}
	for i := range response.Media {
		switch response.Media[i].Type {
		case "image":
			response.Media[i].Type = "photo"
		case "video", "gif":
		default:
			return xPost{}, fmt.Errorf("unsupported VxTwitter media type %q", response.Media[i].Type)
		}
	}
	post := xPost{Text: response.Text, Author: response.UserName, Media: response.Media}
	if err := validateXPost(post, "VxTwitter"); err != nil {
		return xPost{}, err
	}
	return post, nil
}

func xPhotoURL(rawURL string) string {
	photoURL, err := url.Parse(rawURL)
	if err != nil || photoURL.Hostname() != "pbs.twimg.com" || !strings.HasPrefix(photoURL.Path, "/media/") {
		return rawURL
	}
	query := photoURL.Query()
	query.Set("name", "large")
	photoURL.RawQuery = query.Encode()
	return photoURL.String()
}

func validateXPost(post xPost, provider string) error {
	if post.Author == "" {
		return fmt.Errorf("%s response missing author name", provider)
	}
	for i, media := range post.Media {
		if media.URL == "" {
			return fmt.Errorf("%s response missing photo URL at index %d", provider, i)
		}
		switch media.Type {
		case "", "photo", "video", "gif":
		default:
			return fmt.Errorf("unsupported %s media type %q", provider, media.Type)
		}
	}
	return nil
}
