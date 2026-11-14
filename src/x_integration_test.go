package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestXProviderFallback(t *testing.T) {
	original := http.DefaultTransport
	var hosts []string
	http.DefaultTransport = xPostTransport(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		if r.URL.Path != "/user/status/123" || r.URL.RawQuery != "view=1" {
			t.Errorf("unexpected request URL: %s", r.URL)
		}
		body := `{"code":200,"tweet":{"author":{"name":"Fx"},"media":{"all":[{"type":"photo"}]}}}`
		if r.URL.Host == "api.vxtwitter.com" {
			body = `{"text":"Post text","user_name":"A & B","media_extended":[
				{"type":"image","url":"https://pbs.twimg.com/media/image.jpg?name=orig"},
				{"type":"video","url":"https://media/video"},
				{"type":"gif","url":"https://media/gif"}]}`
		}
		return &http.Response{
			StatusCode: 200, Status: "OK", Body: io.NopCloser(strings.NewReader(body)),
			Header: make(http.Header), Request: r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })

	link, _ := url.Parse("https://x.com/user/status/123?view=1")
	metadata := &artwork{}
	media, err := handleXLink(link, metadata)
	if strings.Join(hosts, ",") != "api.fxtwitter.com,api.vxtwitter.com" {
		t.Fatalf("requests = %v", hosts)
	}
	if err != nil || len(media) != 3 {
		t.Fatalf("media = %v, error = %v", media, err)
	}
	photo, ok := media[0].(tgbotapi.InputMediaPhoto)
	if !ok || photo.Media != tgbotapi.FileURL("https://pbs.twimg.com/media/image.jpg?name=large") || photo.Caption != "<a href=\"https://fxtwitter.com/user/status/123?view=1\">A &amp; B</a>" || photo.ParseMode != "HTML" {
		t.Fatalf("photo = %+v", media[0])
	}
	for i, want := range []string{"https://media/video", "https://media/gif"} {
		video, ok := media[i+1].(tgbotapi.InputMediaVideo)
		if !ok || video.Media != tgbotapi.FileURL(want) || video.Caption != "" {
			t.Errorf("video %d = %+v", i, media[i+1])
		}
	}
	if metadata.Author != "A & B" || metadata.Text != "Post text" || link.Host != "fxtwitter.com" {
		t.Fatalf("link or metadata = %s, %+v", link, metadata)
	}
}

func TestXProvidersBothFail(t *testing.T) {
	original := http.DefaultTransport
	var hosts []string
	http.DefaultTransport = xPostTransport(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		status, body := 200, `{"code":200,"tweet":{"author":{"name":"Fx"},"media":{"all":[{"type":"photo"}]}}}`
		if r.URL.Host == "api.vxtwitter.com" {
			status, body = 503, "unavailable"
		}
		return &http.Response{
			StatusCode: status, Status: http.StatusText(status),
			Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })

	link, _ := url.Parse("https://x.com/user/status/123")
	metadata := &artwork{}
	_, err := handleXLink(link, metadata)
	if strings.Join(hosts, ",") != "api.fxtwitter.com,api.vxtwitter.com" {
		t.Fatalf("requests = %v", hosts)
	}
	want := "FxTwitter: FxTwitter response missing photo URL at index 0; " +
		"VxTwitter: fetching X link: HTTP Service Unavailable"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
	if link.Host != "x.com" || metadata.Author != "" || metadata.Text != "" {
		t.Fatalf("failure changed link or metadata: %s, %+v", link, metadata)
	}
}
