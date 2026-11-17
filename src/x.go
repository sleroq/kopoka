package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var xLinkHosts = map[string]bool{
	"x.com": true, "twitter.com": true, "fxtwitter.com": true, "fixupx.com": true,
	"vxtwitter.com": true, "fixvx.com": true, "twittpr.com": true, "xfixup.com": true,
	"twitterez.com": true, "xeezz.com": true, "mbedfx.app": true,
	"megapenispoopenfarten.sex": true, "forsen.sex": true, "sorryifuckedyour.mom": true,
	"hotsinglesnear.you": true, "girlcockx.com": true, "hitlerx.com": true,
	"peepeepoopoodumdumtwitterx.org": true, "skibidix.com": true, "yiffx.com": true,
	"imthehottest18yearoldononlyfansx.com": true, "cunnyx.com": true, "boycuntx.com": true,
	"autistic.kids": true, "yaoisex.com": true, "niggerfaggotx.com": true, "gockx.com": true,
	"boypussyx.com": true, "forsenx.com": true, "ucobx.com": true, "catgirlsex.com": true,
	"stupidpenisx.com": true, "hotyurisex.com": true, "mpregx.com": true,
	"furryfeetx.com": true, "jobna.me": true, "boberx.com": true, "catgirlx.com": true,
	"chudx.com": true, "dammitx.com": true, "didyx.com": true, "dormiex.com": true,
	"elonmuskisan.uglyunfunnynazimanchildwithabrokenpenisx.com": true, "enbysex.com": true,
	"epsteinx.com": true, "fatbitchesx.com": true, "femboysx.com": true, "fixgirldix.com": true,
	"fuckelonx.com": true, "glorpx.com": true, "goatsx.com": true, "goyimx.com": true,
	"gubbyx.com": true, "hotyaoisex.com": true, "imthehottest18year01donon1yfansx.com": true,
	"juicyhagcunny.org": true, "kittycockx.com": true, "igb45.com": true,
	"lilgayboy45.com": true, "egapenispoopenfarten.sex": true, "meowerx.com": true,
	"pankomanko.com": true, "pawbsx.com": true, "pinkcatx.com": true,
	"pregnantmanx.com": true, "puppygirlsex.com": true, "samekosabax.com": true,
	"sandycheekscockvorex.com": true, "shimonx.com": true, "silver.gay": true,
}

type xMedia struct {
	Type         string   `json:"type"`
	URL          string   `json:"url"`
	FallbackURLs []string `json:"-"`
}

type xPost struct {
	Text   string
	Author string
	Media  []xMedia
}

func xPhotoIndex(link *url.URL) int {
	parts := strings.Split(link.Path, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] == "" || parts[2] != "status" || parts[4] != "photo" {
		return 0
	}
	id, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil || id == 0 {
		return 0
	}
	n, err := strconv.Atoi(parts[5])
	if err != nil || n <= 0 || strings.Trim(parts[5], "0123456789") != "" {
		return 0
	}
	return n
}

func xPostLink(link *url.URL) *url.URL {
	copy := *link
	if xPhotoIndex(link) > 0 {
		copy.Path = strings.Join(strings.Split(link.Path, "/")[:4], "/")
		copy.RawPath = ""
	}
	return &copy
}

var errXPostNotFound = errors.New("post not found (404)")

type unavailablePhotoError struct{ index int }

func (e unavailablePhotoError) Error() string { return fmt.Sprintf("photo %d is unavailable", e.index) }

func selectXPhoto(post *xPost, link *url.URL) error {
	index := xPhotoIndex(link)
	if index == 0 {
		return nil
	}
	if index > len(post.Media) || (post.Media[index-1].Type != "" && post.Media[index-1].Type != "photo") || post.Media[index-1].URL == "" {
		return unavailablePhotoError{index}
	}
	post.Media = post.Media[index-1 : index]
	return nil
}

func fetchXPost(link *url.URL, allowMissingURLs bool) (xPost, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	fx, fxErr := fetchFxPost(client, link)
	if fxErr == nil {
		return fx, nil
	}
	vx, vxErr := fetchVxPost(client, link)
	if vxErr == nil {
		return vx, nil
	}
	if allowMissingURLs {
		if validateXPostForLink(fx, "FxTwitter", link, false) == nil {
			return fx, nil
		}
		if validateXPostForLink(vx, "VxTwitter", link, false) == nil {
			return vx, nil
		}
	}
	if errors.Is(fxErr, errXPostNotFound) || errors.Is(vxErr, errXPostNotFound) {
		return xPost{}, errXPostNotFound
	}
	var unavailable unavailablePhotoError
	if errors.As(fxErr, &unavailable) {
		return xPost{}, fxErr
	}
	if errors.As(vxErr, &unavailable) {
		return xPost{}, vxErr
	}
	return xPost{}, fmt.Errorf("FxTwitter: %w; VxTwitter: %w", fxErr, vxErr)
}

func fetchXResponse(client *http.Client, link *url.URL, host string, response any) error {
	apiLink := *xPostLink(link)
	apiLink.Host = host
	res, err := client.Get(apiLink.String())
	if err != nil {
		return fmt.Errorf("fetching X link: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return errXPostNotFound
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching X link: HTTP %s", res.Status)
	}
	if err := json.NewDecoder(res.Body).Decode(response); err != nil {
		return fmt.Errorf("decoding X link response: %w", err)
	}
	return nil
}

func fetchFxPost(client *http.Client, link *url.URL) (xPost, error) {
	type fxMedia struct {
		All    []xMedia `json:"all"`
		Photos []xMedia `json:"photos"`
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Tweet   *struct {
			Text   string `json:"text"`
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Media fxMedia `json:"media"`
			Quote struct {
				Media fxMedia `json:"media"`
			} `json:"quote"`
		} `json:"tweet"`
	}
	if err := fetchXResponse(client, link, "api.fxtwitter.com", &response); err != nil {
		return xPost{}, err
	}
	if response.Code == http.StatusNotFound {
		return xPost{}, errXPostNotFound
	}
	if response.Code != http.StatusOK || response.Tweet == nil {
		return xPost{}, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}
	items := response.Tweet.Media.All
	if items == nil {
		items = response.Tweet.Media.Photos
	}
	if len(items) == 0 {
		items = response.Tweet.Quote.Media.All
		if items == nil {
			items = response.Tweet.Quote.Media.Photos
		}
	}
	post := xPost{Text: response.Tweet.Text, Author: response.Tweet.Author.Name, Media: items}
	if err := validateXPostForLink(post, "FxTwitter", link, true); err != nil {
		return post, err
	}
	return post, nil
}

func fetchVxPost(client *http.Client, link *url.URL) (xPost, error) {
	var response struct {
		Text     string   `json:"text"`
		UserName string   `json:"user_name"`
		Media    []xMedia `json:"media_extended"`
		Quote    struct {
			Media []xMedia `json:"media_extended"`
		} `json:"qrt"`
	}
	if err := fetchXResponse(client, link, "api.vxtwitter.com", &response); err != nil {
		return xPost{}, err
	}
	if len(response.Media) == 0 {
		response.Media = response.Quote.Media
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
	if err := validateXPostForLink(post, "VxTwitter", link, true); err != nil {
		return post, err
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

func validateXPostForLink(post xPost, provider string, link *url.URL, requireURLs bool) error {
	if xPhotoIndex(link) > 0 {
		if err := selectXPhoto(&post, link); err != nil {
			return err
		}
	}
	return validateXPost(post, provider, requireURLs)
}

func validateXPost(post xPost, provider string, requireURLs bool) error {
	if post.Author == "" {
		return fmt.Errorf("%s response missing author name", provider)
	}
	for i, media := range post.Media {
		if requireURLs && media.URL == "" {
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
