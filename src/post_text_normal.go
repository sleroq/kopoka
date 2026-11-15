package main

import (
	"html"
	"strings"
	"unicode/utf16"
)

type normalPostPart struct {
	html     string
	text     string
	postText bool
}

func xPostLinkHTML(post *fxRichPost, label string) string {
	label = html.EscapeString(label)
	link := inlineLink(post.URL)
	if link == nil || !xLinkHosts[link.Host] {
		link = inlineLink("https://x.com/" + post.Author.ScreenName + "/status/" + post.ID)
	}
	if link == nil {
		return label
	}
	return `<a href="` + html.EscapeString(link.String()) + `">` + label + `</a>`
}

func xPostAuthorHTML(post *fxRichPost) string {
	return xPostLinkHTML(post, post.Author.Name)
}

func normalXPost(post *fxRichPost, parents []*fxRichPost, more, hasMedia bool) string {
	var parts []normalPostPart
	add := func(markup, plain string) { parts = append(parts, normalPostPart{html: markup, text: plain}) }
	addText := func(value string) { parts = append(parts, normalPostPart{text: value, postText: true}) }
	addAuthor := func(p *fxRichPost) { add(xPostAuthorHTML(p), p.Author.Name) }

	if len(parents) > 0 || more {
		add("Reply thread from "+xPostLinkHTML(post, "X")+":\n", "Reply thread from X:\n")
		if more {
			add("…\n", "…\n")
		}
		for _, parent := range parents {
			add("<blockquote expandable>", "")
			addAuthor(parent)
			if parent.Text != "" {
				add(":\n", ":\n")
				addText(parent.Text)
			}
			add("</blockquote>\n", "\n")
		}
		add("\n", "\n")
	}
	addAuthor(post)
	add(" on X", " on X")
	if post.Text != "" {
		add("\n\n", "\n\n")
		addText(post.Text)
	}
	if post.Quote != nil {
		add("\n\n<blockquote>", "\n\n")
		addAuthor(post.Quote)
		if post.Quote.Text != "" {
			add("\n", "\n")
			addText(post.Quote.Text)
		}
		add("</blockquote>", "")
	}

	limit := 4096
	if hasMedia {
		limit = 1024
	}
	return normalPostHTML(parts, limit)
}

func normalPostLength(parts []normalPostPart) int {
	n := 0
	for _, part := range parts {
		n += len(utf16.Encode([]rune(part.text)))
	}
	return n
}

func normalPostHTML(parts []normalPostPart, limit int) string {
	// Preserve markup and links; remove excess from post bodies, starting with ancestors.
	excess := normalPostLength(parts) - limit
	for i := range parts {
		if excess <= 0 {
			break
		}
		if !parts[i].postText || parts[i].text == "" {
			continue
		}
		runes := []rune(parts[i].text)
		units := len(utf16.Encode(runes))
		keep := max(0, units-excess-1)
		end, used := 0, 0
		for end < len(runes) {
			width := 1
			if runes[end] > 0xffff {
				width = 2
			}
			if used+width > keep {
				break
			}
			used += width
			end++
		}
		parts[i].text = string(runes[:end]) + "…"
		excess -= units - used - 1
	}
	var out strings.Builder
	for _, part := range parts {
		if part.postText {
			out.WriteString(html.EscapeString(part.text))
		} else {
			out.WriteString(part.html)
		}
	}
	return out.String()
}
