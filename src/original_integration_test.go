package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/everpcpc/pixiv"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type originalMedia struct {
	Type  string `json:"type"`
	Media string `json:"media"`
}

type originalRequest struct {
	media []originalMedia
	files map[string][]byte
}

type originalDeliveryServer struct {
	t                  *testing.T
	server             *httptest.Server
	requests           []originalRequest
	downloads, reports []string
	images             map[string][]byte
	rejectUploads      bool
}

func (s *originalDeliveryServer) telegram(w http.ResponseWriter, r *http.Request, group bool) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			s.t.Error(err)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	} else if err := r.ParseForm(); err != nil {
		s.t.Error(err)
		return
	}

	if r.FormValue("chat_id") != "42" || r.FormValue("reply_to_message_id") != "7" {
		s.t.Errorf("routing: %v", r.Form)
	}

	req := originalRequest{files: make(map[string][]byte)}
	if group {
		if err := json.Unmarshal([]byte(r.FormValue("media")), &req.media); err != nil {
			s.t.Error(err)
			return
		}
		if len(req.media) < 2 || len(req.media) > 10 {
			s.t.Errorf("invalid album size: %d", len(req.media))
		}
	} else {
		req.media = []originalMedia{{Type: "document", Media: r.FormValue("document")}}
	}

	if r.MultipartForm != nil {
		for name, headers := range r.MultipartForm.File {
			for _, header := range headers {
				file, err := header.Open()
				if err != nil {
					s.t.Error(err)
					return
				}
				data, err := io.ReadAll(file)
				_ = file.Close()
				if err != nil {
					s.t.Error(err)
					return
				}
				req.files[name] = data
				if !group && name == "document" {
					req.media[0].Media = "attach://document"
				}
			}
		}
	}

	s.requests = append(s.requests, req)
	if s.rejectUploads && len(req.files) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"upload rejected"}`)
		return
	}
	for _, m := range req.media {
		if m.Type != "document" {
			s.t.Errorf("media type = %q", m.Type)
		}
		if strings.Contains(m.Media, "fallback") || strings.Contains(m.Media, "oversize") || strings.Contains(m.Media, "broken") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"URL rejected"}`)
			return
		}
	}

	if group {
		_, _ = fmt.Fprint(w, `{"ok":true,"result":[{"message_id":1},{"message_id":2}]}`)
	} else {
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
	}
}

func (s *originalDeliveryServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/bottoken/getMe":
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`)
	case "/bottoken/sendDocument":
		s.telegram(w, r, false)
	case "/bottoken/sendMediaGroup":
		s.telegram(w, r, true)
	case "/bottoken/sendMessage":
		_ = r.ParseForm()
		if r.FormValue("parse_mode") != "HTML" || r.FormValue("chat_id") != "42" || r.FormValue("reply_to_message_id") != "7" {
			s.t.Errorf("report routing: %v", r.Form)
		}

		s.reports = append(s.reports, r.FormValue("text"))
		_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":2}}`)
	case "/user/status/1":
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[
 {"type":"photo","url":"https://pbs.twimg.com/media/a.jpg?format=jpg&name=small"},
 {"type":"video","url":"%s/fallback.mp4"},
 {"type":"photo","url":"%s/oversize.jpg?a=1&b=2"},
 {"type":"gif","url":"%s/broken.gif"},{"type":"video","url":"%s/last.mp4"}]}}}`, s.server.URL, s.server.URL, s.server.URL, s.server.URL)
	case "/user/status/3":
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[
 {"type":"photo"},{"type":"video","url":"%s/last.mp4"}]}}}`, s.server.URL)
	case "/user/status/4", "/user/status/5":
		count := 5
		if r.URL.Path == "/user/status/5" {
			count = 11
		}
		var media []string
		for i := range count {
			photo := fmt.Sprintf(`{"type":"photo","url":"https://pbs.twimg.com/media/%d.jpg?format=jpg&name=small"}`, i)
			media = append(media, photo)
		}
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[%s]}}}`, strings.Join(media, ","))
	case "/user/status/6":
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[
 {"type":"photo","url":"%s/panel0.png"},{"type":"photo","url":"%s/panel1.png"},
 {"type":"video","url":"%s/last.mp4"},{"type":"photo","url":"%s/independent.png"}]}}}`, s.server.URL, s.server.URL, s.server.URL, s.server.URL)
	case "/user/status/7":
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[
 {"type":"photo","url":"%s/panel0.png"},{"type":"photo","url":"%s/panel1.png"}]}}}`, s.server.URL, s.server.URL)
	case "/user/status/8":
		_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{"author":{"name":"a"},"media":{"all":[
 {"type":"photo","url":"%s/preview-unavailable0.png"},{"type":"photo","url":"%s/preview-unavailable1.png"}]}}}`, s.server.URL, s.server.URL)
	case "/auth/token":
		_, _ = fmt.Fprint(w, `{"response":{"access_token":"access","refresh_token":"refresh","expires_in":3600,"user":{}}}`)
	case "/v1/illust/detail":
		if r.URL.Query().Get("illust_id") == "2" {
			_, _ = fmt.Fprint(w, `{"illust":{"type":"ugoira"}}`)
			return
		}
		if r.URL.Query().Get("illust_id") == "4" {
			_, _ = fmt.Fprintf(w, `{"illust":{"type":"illust","meta_pages":[
 {"image_urls":{"original":"%s/original0.png","large":"%s/preview0.png"}},
 {"image_urls":{"original":"%s/original1.png","large":"%s/preview1.png"}}]}}`,
				s.server.URL, s.server.URL, s.server.URL, s.server.URL)
			return
		}
		_, _ = fmt.Fprintf(w, `{"illust":{"type":"illust","meta_pages":[
 {"image_urls":{"original":"%s/p0.jpg","large":"%s/large.jpg"}},
 {"image_urls":{}},{"image_urls":{"original":"%s/p1.jpg"}}]}}`, s.server.URL, s.server.URL, s.server.URL)
	case "/v1/ugoira/metadata":
		_, _ = fmt.Fprintf(w, `{"ugoira_metadata":{"zip_urls":{"medium":"%s/123_ugoira600x600.zip"}}}`, s.server.URL)
	default:
		s.downloads = append(s.downloads, r.URL.Path)
		if (r.URL.Path == "/p0.jpg" || r.URL.Path == "/p1.jpg" || strings.HasPrefix(r.URL.Path, "/original") || strings.HasSuffix(r.URL.Path, ".zip")) && r.Referer() != "https://www.pixiv.net/" {
			s.t.Errorf("referer = %q", r.Referer())
		}
		switch r.URL.Path {
		case "/broken.gif", "/preview-unavailable0.png":
			http.Error(w, "unavailable", 500)
		case "/oversize.jpg":
			w.Header().Set("Content-Length", fmt.Sprint(originalFileLimit+1))
		case "/fallback.mp4", "/last.mp4", "/p0.jpg", "/p1.jpg", "/123_ugoira1920x1080.zip":
			_, _ = fmt.Fprint(w, "bytes")
		default:
			if data, ok := s.images[r.URL.Path]; ok {
				_, _ = w.Write(data)
			} else {
				s.t.Errorf("unexpected request %s", r.URL)
				http.NotFound(w, r)
			}
		}
	}
}

func setupOriginalDelivery(t *testing.T) (*tgbotapi.BotAPI, *originalDeliveryServer) {
	t.Helper()
	api := &originalDeliveryServer{t: t, images: make(map[string][]byte)}
	api.server = httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	t.Cleanup(api.server.Close)
	target, _ := url.Parse(api.server.URL)
	old := http.DefaultTransport
	http.DefaultTransport = xPostTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.fxtwitter.com" || req.URL.Host == "api.vxtwitter.com" || req.URL.Host == "app-api.pixiv.net" || req.URL.Host == "oauth.secure.pixiv.net" {
			copy := req.Clone(req.Context())
			u := *req.URL
			u.Scheme, u.Host = target.Scheme, target.Host
			copy.URL = &u
			return old.RoundTrip(copy)
		}
		return old.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	bot, err := tgbotapi.NewBotAPIWithClient("token", api.server.URL+"/bot%s/%s", api.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, api
}

func deliverOriginal(t *testing.T, bot *tgbotapi.BotAPI, app *pixiv.AppPixivAPI, raw string, saved *artwork) {
	t.Helper()
	link, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = sendOriginalFiles(bot, app, 42, 7, link, saved); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalURLAlbums(t *testing.T) {
	for _, tc := range []struct {
		id      string
		count   int
		batches []int
	}{{"4", 5, []int{5}}, {"5", 11, []int{9, 2}}} {
		t.Run(tc.id, func(t *testing.T) {
			bot, api := setupOriginalDelivery(t)
			deliverOriginal(t, bot, nil, "https://x.com/user/status/"+tc.id, &artwork{Photos: make([]string, tc.count)})
			if len(api.requests) != len(tc.batches) {
				t.Fatalf("requests: %+v", api.requests)
			}
			index := 0
			for i, req := range api.requests {
				if len(req.media) != tc.batches[i] || len(req.files) != 0 {
					t.Fatalf("album: %+v", req)
				}
				for _, m := range req.media {
					want := fmt.Sprintf("https://pbs.twimg.com/media/%d.jpg?format=jpg&name=orig", index)
					if m.Media != want {
						t.Errorf("media = %q, want %q", m.Media, want)
					}
					index++
				}
			}
			if len(api.downloads) != 0 || len(api.reports) != 0 {
				t.Fatalf("downloads %v reports %v", api.downloads, api.reports)
			}
		})
	}
}

func TestOriginalURLAlbumFallback(t *testing.T) {
	bot, api := setupOriginalDelivery(t)
	// Redirect the Twitter CDN too, so the first URL can be downloaded after rejection.
	old := http.DefaultTransport
	target, _ := url.Parse(api.server.URL)
	http.DefaultTransport = xPostTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "pbs.twimg.com" {
			copy := req.Clone(req.Context())
			u := *req.URL
			u.Scheme, u.Host = target.Scheme, target.Host
			u.Path = "/fallback.mp4"
			copy.URL = &u
			return old.RoundTrip(copy)
		}
		return old.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	deliverOriginal(t, bot, nil, "https://x.com/user/status/1", &artwork{Photos: make([]string, 5)})
	if len(api.requests) != 2 || len(api.requests[0].media) != 5 || len(api.requests[0].files) != 0 || len(api.requests[1].media) != 3 {
		t.Fatalf("requests: %+v", api.requests)
	}
	for _, m := range api.requests[1].media {
		if !strings.HasPrefix(m.Media, "attach://") || string(api.requests[1].files[strings.TrimPrefix(m.Media, "attach://")]) != "bytes" {
			t.Fatalf("fallback upload: %+v", api.requests[1])
		}
	}
	want := `failed files: unable to upload <a href="` + api.server.URL + `/oversize.jpg?a=1&amp;b=2">3rd file</a> for you` + "\n" +
		`failed files: unable to upload <a href="` + api.server.URL + `/broken.gif">4th file</a> for you`
	if !reflect.DeepEqual(api.reports, []string{want}) {
		t.Fatalf("reports = %v", api.reports)
	}
	if !reflect.DeepEqual(api.downloads, []string{"/fallback.mp4", "/fallback.mp4", "/oversize.jpg", "/broken.gif", "/last.mp4"}) {
		t.Fatalf("downloads = %v", api.downloads)
	}
}

func TestOriginalURLsWhenPreviewFails(t *testing.T) {
	bot, api := setupOriginalDelivery(t)
	deliverOriginal(t, bot, nil, "https://x.com/user/status/8", nil)
	if len(api.requests) != 1 || len(api.requests[0].media) != 2 || len(api.requests[0].files) != 0 || len(api.reports) != 0 {
		t.Fatalf("requests %v reports %v", api.requests, api.reports)
	}
	if !reflect.DeepEqual(api.downloads, []string{"/preview-unavailable0.png"}) {
		t.Fatalf("downloads = %v", api.downloads)
	}
}

func TestOriginalPixivAndMissingURL(t *testing.T) {
	bot, api := setupOriginalDelivery(t)
	if _, err := pixiv.LoadAuth("access", "refresh", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	app := pixiv.NewApp()
	deliverOriginal(t, bot, app, "https://www.pixiv.net/artworks/1", &artwork{Photos: make([]string, 3)})
	deliverOriginal(t, bot, app, "https://www.pixiv.net/artworks/2", &artwork{Photos: make([]string, 1)})
	if len(api.requests) != 2 || len(api.requests[0].media) != 2 || len(api.requests[1].media) != 1 {
		t.Fatalf("requests = %+v", api.requests)
	}
	for _, req := range api.requests {
		for _, m := range req.media {
			if !strings.HasPrefix(m.Media, "attach://") || string(req.files[strings.TrimPrefix(m.Media, "attach://")]) != "bytes" {
				t.Fatalf("Pixiv upload = %+v", req)
			}
		}
	}
	if !reflect.DeepEqual(api.downloads, []string{"/p0.jpg", "/p1.jpg", "/123_ugoira1920x1080.zip"}) || !reflect.DeepEqual(api.reports, []string{"failed files: unable to upload 2nd file for you"}) {
		t.Fatalf("downloads %v reports %v", api.downloads, api.reports)
	}
	api.requests, api.reports = nil, nil
	deliverOriginal(t, bot, nil, "https://x.com/user/status/3", &artwork{Photos: make([]string, 2)})
	if len(api.requests) != 1 || len(api.requests[0].media) != 1 || api.requests[0].media[0].Media != api.server.URL+"/last.mp4" || !reflect.DeepEqual(api.reports, []string{"failed files: unable to upload 1st file for you"}) {
		t.Fatalf("requests %v reports %v", api.requests, api.reports)
	}
}

func TestOriginalMergedPanelsAndVideoBoundary(t *testing.T) {
	bot, api := setupOriginalDelivery(t)
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	independent := artScene(image.Rect(0, 0, 200, 320), 2)
	for name, img := range map[string]image.Image{"/panel0.png": scene.SubImage(image.Rect(0, 0, 200, 320)), "/panel1.png": scene.SubImage(image.Rect(200, 0, 400, 320)), "/independent.png": independent} {
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		api.images[name] = b.Bytes()
	}

	deliverOriginal(t, bot, nil, "https://x.com/user/status/6", &artwork{Photos: make([]string, 3)})
	if len(api.requests) != 1 || len(api.requests[0].media) != 3 || len(api.reports) != 0 {
		t.Fatalf("requests %v reports %v", api.requests, api.reports)
	}

	req := api.requests[0]
	first := req.media[0].Media
	if !strings.HasPrefix(first, "attach://") {
		t.Fatalf("joined image is not uploaded: %+v", req)
	}
	joined, err := png.Decode(bytes.NewReader(req.files[strings.TrimPrefix(first, "attach://")]))
	if err != nil {
		t.Fatal(err)
	}
	checkOriginalPixels(t, joined, scene)

	if req.media[1].Media != api.server.URL+"/last.mp4" {
		t.Fatalf("video = %s", req.media[1].Media)
	}
	// An independent image must remain a separate document, not join across video.
	if req.media[2].Media != api.server.URL+"/independent.png" {
		t.Fatalf("independent = %s", req.media[2].Media)
	}
}

func TestOriginalMergedPanelsSingleDocument(t *testing.T) {
	bot, api := setupOriginalDelivery(t)
	scene := artScene(image.Rect(0, 0, 400, 320), 0)
	for i := range 2 {
		var b bytes.Buffer
		panel := scene.SubImage(image.Rect(i*200, 0, (i+1)*200, 320))
		if err := png.Encode(&b, panel); err != nil {
			t.Fatal(err)
		}
		api.images[fmt.Sprintf("/panel%d.png", i)] = b.Bytes()
	}

	// A nil saved artwork also triggers original panel assembly.
	deliverOriginal(t, bot, nil, "https://x.com/user/status/7", nil)
	if len(api.requests) != 1 || len(api.requests[0].media) != 1 || len(api.reports) != 0 {
		t.Fatalf("requests %v reports %v", api.requests, api.reports)
	}
	req := api.requests[0]
	if req.media[0].Media != "attach://document" {
		t.Fatalf("expected multipart document: %+v", req)
	}
	joined, err := png.Decode(bytes.NewReader(req.files["document"]))
	if err != nil {
		t.Fatal(err)
	}
	checkOriginalPixels(t, joined, scene)

	api.requests = nil
	api.rejectUploads = true
	deliverOriginal(t, bot, nil, "https://x.com/user/status/7", nil)
	want := `failed files: unable to upload <a href="` + api.server.URL + `/panel0.png">1st file</a> for you` + "\n" +
		`failed files: unable to upload <a href="` + api.server.URL + `/panel1.png">2nd file</a> for you`
	if len(api.requests) != 1 || !reflect.DeepEqual(api.reports, []string{want}) {
		t.Fatalf("joined upload failure: requests %v reports %v", api.requests, api.reports)
	}
}

func TestOriginalUsesPreviewLayout(t *testing.T) {
	bot, api := setupOriginalDelivery(t)
	if _, err := pixiv.LoadAuth("access", "refresh", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	preview := artScene(image.Rect(0, 0, 400, 320), 0)
	left := artScene(image.Rect(0, 0, 400, 640), 0)
	right := artScene(image.Rect(0, 0, 400, 640), 3)
	left.SetRGBA(10, 10, color.RGBA{})
	right.SetRGBA(20, 20, color.RGBA{R: 64, A: 128})
	if panels := joinArtPanels([]image.Image{left, right}); len(panels) != 2 {
		t.Fatalf("original panels unexpectedly match: %d", len(panels))
	}
	for name, img := range map[string]image.Image{
		"/preview0.png":  preview.SubImage(image.Rect(0, 0, 200, 320)),
		"/preview1.png":  preview.SubImage(image.Rect(200, 0, 400, 320)),
		"/original0.png": left, "/original1.png": right,
	} {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, img); err != nil {
			t.Fatal(err)
		}
		api.images[name] = encoded.Bytes()
	}

	app := pixiv.NewApp()
	saved := &artwork{Photos: []string{"merged-preview"}}
	deliverOriginal(t, bot, app, "https://www.pixiv.net/artworks/4", saved)
	if len(api.requests) != 1 || len(api.requests[0].media) != 1 || len(api.reports) != 0 {
		t.Fatalf("requests %v reports %v", api.requests, api.reports)
	}
	joined, err := png.Decode(bytes.NewReader(api.requests[0].files["document"]))
	if err != nil {
		t.Fatal(err)
	}
	expected := image.NewNRGBA(image.Rect(0, 0, 800, 640))
	for i := range 2 {
		source, err := png.Decode(bytes.NewReader(api.images[fmt.Sprintf("/original%d.png", i)]))
		if err != nil {
			t.Fatal(err)
		}
		draw.Draw(expected, image.Rect(i*400, 0, (i+1)*400, 640), source, image.Point{}, draw.Src)
	}
	checkOriginalPixels(t, joined, expected)
}

func checkOriginalPixels(t *testing.T, joined, scene image.Image) {
	t.Helper()
	if joined.Bounds() != scene.Bounds() {
		t.Fatalf("joined bounds = %v", joined.Bounds())
	}
	for y := range scene.Bounds().Dy() {
		for x := range scene.Bounds().Dx() {
			r, g, b, a := joined.At(x, y).RGBA()
			wantR, wantG, wantB, wantA := scene.At(x, y).RGBA()
			if r != wantR || g != wantG || b != wantB || a != wantA {
				t.Fatalf("joined pixel at %d,%d = %d,%d,%d,%d; want %d,%d,%d,%d", x, y, r, g, b, a, wantR, wantG, wantB, wantA)
			}
		}
	}
}

func TestOriginalChunkedOversize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		chunk := make([]byte, 64*1024)
		for sent := 0; sent <= originalFileLimit; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	if _, err := downloadOriginal(server.Client(), server.URL+"/original.png", true); err == nil {
		t.Fatal("oversized chunked download succeeded")
	}
}
