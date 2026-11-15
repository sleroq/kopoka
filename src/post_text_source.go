package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
)

func loadXPostThread(client *http.Client, link *url.URL) (post *fxRichPost, parents []*fxRichPost, more bool, err error) {
	postLink := xPostLink(link)
	post, err = fetchRichPost(client, postLink)
	if err != nil {
		return nil, nil, false, err
	}
	if post.Author.Name == "" {
		return nil, nil, false, fmt.Errorf("FxTwitter response missing author name")
	}
	if post.URL == "" {
		post.URL = postLink.String()
	}

	current := post
	for len(parents) < 3 && current.ReplyingToStatus != "" {
		id, parseErr := strconv.ParseUint(current.ReplyingToStatus, 10, 64)
		if parseErr != nil || id == 0 {
			log.Printf("invalid FxTwitter reply parent %q", current.ReplyingToStatus)
			more = true
			break
		}
		parentLink := &url.URL{Scheme: "https", Host: "x.com", Path: "/i/status/" + strconv.FormatUint(id, 10)}
		parent, fetchErr := fetchRichPost(client, parentLink)
		if fetchErr != nil {
			log.Printf("fetching FxTwitter reply parent %s: %v", parentLink, fetchErr)
			more = true
			break
		}
		if parent.Author.Name == "" {
			log.Printf("FxTwitter reply parent %s missing author name", parentLink)
			more = true
			break
		}
		if parent.URL == "" {
			parent.URL = parentLink.String()
		}
		parents = append(parents, parent)
		current = parent
	}
	more = more || current.ReplyingToStatus != ""
	for i, j := 0, len(parents)-1; i < j; i, j = i+1, j-1 {
		parents[i], parents[j] = parents[j], parents[i]
	}
	return post, parents, more, nil
}
