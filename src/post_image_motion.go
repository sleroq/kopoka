package main

import (
	"encoding/json"
	"net/url"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (p renderPost) hasMotion() bool {
	if p.Media != nil {
		for _, item := range p.Media.Items {
			if item.Type == "gif" || item.Type == "video" {
				return true
			}
		}
	}
	return p.Quote != nil && p.Quote.hasMotion()
}

func (r renderRequest) hasMotion() bool {
	if r.Post.hasMotion() {
		return true
	}
	for _, post := range r.ReplyChain {
		if post.hasMotion() {
			return true
		}
	}
	return false
}

func renderPostMedia(link *url.URL, request renderRequest) (any, error) {
	base := tgbotapi.BaseInputMedia{Type: "photo", Caption: postImageCaption(link, request), ParseMode: "HTML"}
	if request.hasMotion() {
		request.Scene = true
		data, err := runPostRenderer(request)
		if err != nil {
			return nil, err
		}
		var scene motionScene
		if err := json.Unmarshal(data, &scene); err != nil {
			return nil, err
		}
		file, kind, err := renderMotion(scene)
		if err != nil {
			return nil, err
		}
		base.Type, base.Media = kind, file
	} else {
		data, err := renderPostImage(request)
		if err != nil {
			return nil, err
		}
		base.Media = tgbotapi.FileBytes{Name: "post.jpg", Bytes: data}
	}
	return postImageInput(base), nil
}

func postImageInput(base tgbotapi.BaseInputMedia) any {
	switch base.Type {
	case "animation":
		return tgbotapi.InputMediaAnimation{BaseInputMedia: base}
	case "video":
		return tgbotapi.InputMediaVideo{BaseInputMedia: base, SupportsStreaming: true}
	default:
		return tgbotapi.InputMediaPhoto{BaseInputMedia: base}
	}
}
