package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestXPollThreadPropagation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poll := `{
			"choices": [{"label":"A <&>","percentage":25.5}, {"label":"B","percentage":74.5}],
			"total_votes":123, "time_left_en":%q
		}`

		mainPoll := fmt.Sprintf(poll, "21 hours")
		quotePoll := fmt.Sprintf(poll, "Final results")
		parentPoll := fmt.Sprintf(poll, "2 & days left")
		switch r.URL.Path {
		case "/user/status/5", "/user/status/5/en":
			_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{
				"text":"Main", "author":{"name":"Main","screen_name":"user"},
				"poll":%s, "replying_to_status":"4",
				"quote":{"text":"Quote","author":{"name":"Quote","screen_name":"q"},"poll":%s}
			}}`, mainPoll, quotePoll)
		case "/i/status/4", "/i/status/4/en":
			_, _ = fmt.Fprintf(w, `{"code":200,"tweet":{
				"text":"Parent", "author":{"name":"Parent","screen_name":"p"}, "poll":%s
			}}`, parentPoll)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target, _ := url.Parse(server.URL)
	client := &http.Client{Transport: redirectTransport{target, http.DefaultTransport}}
	link, _ := url.Parse("https://x.com/user/status/5")
	request, err := preparePostImageWithClient(client, link, "dark", "UTC", defaultTranslationSettings())
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Post struct {
			Poll  *postPoll `json:"poll"`
			Quote struct {
				Poll *postPoll `json:"poll"`
			} `json:"quote"`
		} `json:"post"`
		ReplyChain []struct {
			Poll *postPoll `json:"poll"`
		} `json:"reply_chain"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.ReplyChain) != 1 {
		t.Fatalf("reply chain: %s", data)
	}
	for i, poll := range []*postPoll{result.Post.Poll, result.Post.Quote.Poll, result.ReplyChain[0].Poll} {
		want := []string{"21 hours left", "Final results", "2 & days left"}[i]
		if poll == nil || len(poll.Choices) != 2 || poll.Choices[0].Label != "A <&>" || poll.Choices[0].Percentage != 25.5 || poll.TotalVotes != 123 || poll.TimeLeftEN != want {
			t.Fatalf("poll propagation: %s", data)
		}
	}

	post, parents, more, err := loadXPostThread(client, link)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"rich": richXPost(post, parents, more, nil, nil)["html"].(string),
	} {
		for _, want := range []string{"■■■□□□□□□□ A &lt;&amp;&gt; 25.5%", "■■■■■■■□□□ B 74.5%"} {
			if strings.Count(text, want) != 3 {
				t.Errorf("%s missing three copies of %q: %s", name, want, text)
			}
		}
		for _, want := range []string{"123 votes · 21 hours left", "123 votes · Final results", "123 votes · 2 &amp; days left"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q: %s", name, want, text)
			}
		}
	}
}
