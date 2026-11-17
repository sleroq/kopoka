package main

import (
	"bytes"
	"image/jpeg"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type postTextPreviewWork struct {
	done          chan struct{}
	thumbnail     []byte
	large         []byte
	width, height int
	err           error
	finished      time.Time
}

func (t *thumbnails) startPostTextPreview(link *url.URL) *postTextPreviewWork {
	link = xPostLink(link)
	key := artworkID(link)
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, work := range t.postTextWork {
		if !work.finished.IsZero() && time.Since(work.finished) >= 5*time.Minute {
			delete(t.postTextWork, k)
		}
	}
	if work := t.postTextWork[key]; work != nil {
		return work
	}
	work := &postTextPreviewWork{done: make(chan struct{})}
	t.postTextWork[key] = work
	go func() {
		t.workers <- struct{}{}
		blocks, err := t.postTextPreviewBlocks(link)
		if err == nil {
			work.thumbnail, work.large, work.width, work.height, err = t.renderMessagePreview(blocks)
		}
		<-t.workers
		t.mu.Lock()
		work.err, work.finished = err, time.Now()
		if err != nil {
			delete(t.postTextWork, key)
		}
		close(work.done)
		t.mu.Unlock()
	}()
	return work
}

func (t *thumbnails) postTextPreview(link *url.URL, instant bool) previewPhoto {
	if instant {
		return t.postTextPhoto()
	}
	work := t.startPostTextPreview(link)
	select {
	case <-work.done:
		if work.err == nil {
			address := *t.base
			address.Path += "/post-text-preview"
			address.RawQuery = url.Values{
				"url": {xPostLink(link).String()},
				"v":   {strconv.Itoa(thumbnailRenderVersion)},
			}.Encode()
			u := address.String()
			return previewPhoto{photoURL: u + "&size=large", thumbnailURL: u, width: work.width, height: work.height}
		}
		log.Printf("generating post text preview for %s: %v", link, work.err)
	case <-time.After(8 * time.Second):
	}
	return t.postTextPhoto()
}

func (t *thumbnails) servePostTextPreview(w http.ResponseWriter, r *http.Request) {
	link := inlineLink(r.URL.Query().Get("url"))
	size := r.URL.Query().Get("size")
	if link == nil || !xLinkHosts[strings.ToLower(link.Host)] || size != "" && size != "large" {
		http.Error(w, "invalid preview request", http.StatusBadRequest)
		return
	}
	work := t.startPostTextPreview(link)
	select {
	case <-r.Context().Done():
		return
	case <-work.done:
		if work.err != nil {
			log.Printf("generating post text preview for %s: %v", link, work.err)
			http.Error(w, "post text preview generation failed", http.StatusBadGateway)
			return
		}
		data := work.thumbnail
		if size == "large" {
			data = work.large
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}
}

func previewPostMedia(post *fxRichPost) []thumbnailMedia {
	if post == nil || post.Media == nil {
		return nil
	}
	items := post.Media.All
	if len(items) == 0 {
		items = append([]fxImage(nil), post.Media.Photos...)
		for _, video := range post.Media.Videos {
			if video.Type == "" {
				video.Type = "video"
			}
			items = append(items, video)
		}
	}
	media := make([]thumbnailMedia, 0, len(items))
	for _, item := range items {
		media = append(media, thumbnailMedia{Type: item.Type, URL: item.URL, ThumbnailURL: item.ThumbnailURL})
	}
	return media
}

func (t *thumbnails) postTextMediaBlock(items []thumbnailMedia, carousel bool) (previewBlock, error) {
	count := 1
	if carousel {
		count = len(items)
		items = items[:1]
	}
	video := slices.ContainsFunc(items, thumbnailMedia.moving)
	posters := make([]thumbnailMedia, len(items))
	for i, item := range items {
		address := item.URL
		if item.moving() {
			address = item.ThumbnailURL
		}
		if address == "" {
			return previewBlock{Kind: "media", Image: t.postTextIcon, Video: video, Count: count}, nil
		}
		posters[i] = thumbnailMedia{Type: "photo", URL: address}
	}
	source := thumbnailSource{media: posters}
	media, err := t.downloadSource(source)
	if err != nil {
		return previewBlock{}, err
	}
	mode := ""
	if len(posters) > 1 {
		mode = "1"
	}
	img, _, err := t.mergeSource(media, mode)
	if err != nil {
		return previewBlock{}, err
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 85}); err != nil {
		return previewBlock{}, err
	}
	return previewBlock{Kind: "media", Image: encoded.Bytes(), Video: video, Count: count}, nil
}

func (t *thumbnails) postTextPreviewBlocks(link *url.URL) ([]previewBlock, error) {
	post, parents, more, err := loadXPostThread(t.client, link)
	if err != nil {
		return nil, err
	}
	for _, p := range []*fxRichPost{post, post.Quote} {
		if _, err := xPostAttachments(p); err != nil {
			return nil, err
		}
	}
	own, quote := previewPostMedia(post), previewPostMedia(post.Quote)
	return t.richPostPreviewBlocks(post, parents, more, own, quote)
}

func previewPostQuote(post *fxRichPost, poll string) previewBlock {
	text := post.Author.Name
	if post.Text != "" {
		text += "\n" + post.Text
	}
	if poll != "" {
		text += "\n" + poll
	}
	return previewBlock{Kind: "quote", Text: text}
}

func previewReplyThread(parents []*fxRichPost, more bool, polls map[*fxRichPost]string) []previewBlock {
	if len(parents) == 0 && !more {
		return nil
	}
	blocks := []previewBlock{{Kind: "text", Text: "Reply thread on X:"}}
	if more {
		blocks = append(blocks, previewBlock{Kind: "text", Text: "…"})
	}
	for _, parent := range parents {
		blocks = append(blocks, previewPostQuote(parent, polls[parent]))
	}
	return blocks
}

func (t *thumbnails) richPostPreviewBlocks(post *fxRichPost, parents []*fxRichPost, more bool, own, quote []thumbnailMedia) ([]previewBlock, error) {
	polls := limitRichPostText(post, parents)
	blocks := previewReplyThread(parents, more, polls)
	author := post.Author.Name + " on X"
	if len(parents) > 0 || more {
		author = post.Author.Name + ":"
	}
	blocks = append(blocks, previewBlock{Kind: "author", Text: author})
	if post.Text != "" {
		blocks = append(blocks, previewBlock{Kind: "text", Text: post.Text})
	}
	if polls[post] != "" {
		blocks = append(blocks, previewBlock{Kind: "text", Text: polls[post]})
	}
	if len(own) > 0 {
		media, err := t.postTextMediaBlock(own, true)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, media)
	}
	if post.Quote != nil {
		blocks = append(blocks, previewPostQuote(post.Quote, polls[post.Quote]))
	}
	if len(quote) > 0 {
		media, err := t.postTextMediaBlock(quote, true)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, media)
	}
	return blocks, nil
}
