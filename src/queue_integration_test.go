package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func queuedChat(user int64, messageID int, ids ...int) *tgbotapi.Message {
	message := &tgbotapi.Message{MessageID: messageID, Chat: &tgbotapi.Chat{ID: 123}, From: &tgbotapi.User{ID: user}}
	for _, id := range ids {
		if message.Text != "" {
			message.Text += " "
		}
		link := fmt.Sprintf("https://x.com/u/status/%d", id)
		entity := tgbotapi.MessageEntity{Type: "url", Offset: len(message.Text), Length: len(link)}
		message.Entities = append(message.Entities, entity)
		message.Text += link
	}
	return message
}

func queueEvent(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
		return ""
	}
}

func waitForQueued(t *testing.T, queue *mediaQueue, user int64, count int) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	for {
		queue.mu.Lock()
		usage := queue.users[user]
		queued := 0
		if usage != nil {
			queued = usage.queued
		}
		queue.mu.Unlock()
		if queued == count {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("user %d has %d queued jobs, want %d", user, queued, count)
		}
	}
}

type queueTestAPI struct {
	t                        *testing.T
	started, feedback, edits chan string
	permit, unblock          chan struct{}
	unblockOnce              sync.Once
	block                    bool
	failPath                 string
	active, maximum          atomic.Int32
	mu                       sync.Mutex
	fetched                  []string
}

func newQueueTestAPI(t *testing.T, block bool) (*queueTestAPI, *tgbotapi.BotAPI) {
	t.Helper()
	api := &queueTestAPI{
		t: t, block: block, started: make(chan string, 32), feedback: make(chan string, 16),
		edits: make(chan string, 16), permit: make(chan struct{}, 8), unblock: make(chan struct{}),
	}
	server := httptest.NewServer(api)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = redirectTransport{target, original}
	t.Cleanup(func() {
		api.release()
		http.DefaultTransport = original
		server.Close()
	})
	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", server.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	return api, bot
}

func (api *queueTestAPI) release() {
	api.unblockOnce.Do(func() { close(api.unblock) })
}

func (api *queueTestAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch {
	case strings.HasPrefix(r.URL.Path, "/u/status/"):
		api.fetch(w, r)
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot"}}`))
	case strings.HasSuffix(r.URL.Path, "/setMessageReaction"), strings.HasSuffix(r.URL.Path, "/sendChatAction"), strings.HasSuffix(r.URL.Path, "/deleteMessage"), strings.HasSuffix(r.URL.Path, "/editMessageMedia"):
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":43,"photo":[{"file_id":"file"}]}}`))
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		api.feedback <- r.FormValue("reply_to_message_id") + ":" + r.FormValue("text")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":44}}`))
	case strings.HasSuffix(r.URL.Path, "/editMessageText"):
		api.edits <- r.FormValue("inline_message_id") + ":" + r.FormValue("text")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	default:
		api.t.Errorf("unexpected request %s", r.URL.Path)
	}
}

func (api *queueTestAPI) fetch(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	api.fetched = append(api.fetched, r.URL.Path)
	api.mu.Unlock()
	n := api.active.Add(1)
	for old := api.maximum.Load(); n > old && !api.maximum.CompareAndSwap(old, n); old = api.maximum.Load() {
	}
	api.started <- r.URL.Path
	if api.block {
		select {
		case <-api.permit:
		case <-api.unblock:
		}
	}
	api.active.Add(-1)
	if r.URL.Path == api.failPath {
		http.Error(w, "upstream failure", http.StatusBadGateway)
		return
	}
	response := `{"code":200,"tweet":{"author":{"name":"Artist"},` +
		`"media":{"photos":[{"url":"https://example.com/photo"}]}}}`
	_, _ = w.Write([]byte(response))
}

func TestMediaQueueChatAndInline(t *testing.T) {
	api, bot := newQueueTestAPI(t, true)
	db := testArtworkDB(t)
	queue := newMediaQueue(mediaLimits{concurrency: 3, queuePerUser: 2, perHour: 100, perDay: 600})
	finished := make(chan string, 16)
	var wg sync.WaitGroup
	defer func() { api.release(); wg.Wait() }()
	launch := func(name string, work func()) {
		wg.Go(func() { work(); finished <- name })
	}

	launch("batch", func() { handleMessage(bot, nil, db, queuedChat(1, 42, 1, 2, 3, 4, 5, 6), queue, nil) })
	first := make(map[string]bool)
	for range 3 {
		first[queueEvent(t, api.started)] = true
	}
	for _, path := range []string{"/u/status/1", "/u/status/2", "/u/status/3"} {
		if !first[path] {
			t.Fatalf("first started: %v", first)
		}
	}
	message := queueEvent(t, api.feedback)
	if !strings.HasPrefix(message, "42:") || !strings.Contains(message, "1") || !strings.Contains(strings.ToLower(message), "queue") || !strings.Contains(strings.ToLower(message), "retry later") {
		t.Fatalf("batch rejection: %s", message)
	}

	launch("forward", func() { handleMessage(bot, nil, db, queuedChat(1, 43, 9), queue, nil) })
	message = queueEvent(t, api.feedback)
	if !strings.HasPrefix(message, "43:") || !strings.Contains(strings.ToLower(message), "queue") {
		t.Fatalf("forward rejection: %s", message)
	}
	if got := queueEvent(t, finished); got != "forward" {
		t.Fatalf("finished before releasing work: %s", got)
	}
	launch("rejected inline", func() {
		handleChosenInlineResult(time.Now(), bot, nil, db, &tgbotapi.ChosenInlineResult{
			From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, InlineMessageID: "rejected", Query: "https://x.com/u/status/10",
		}, "@cache", queue, nil)
	})
	message = queueEvent(t, api.edits)
	if !strings.HasPrefix(message, "rejected:") || !strings.Contains(strings.ToLower(message), "queue") || !strings.Contains(strings.ToLower(message), "retry later") {
		t.Fatalf("inline rejection: %s", message)
	}
	if got := queueEvent(t, finished); got != "rejected inline" {
		t.Fatalf("finished before releasing work: %s", got)
	}

	launch("other chat", func() { handleMessage(bot, nil, db, queuedChat(2, 44, 7), queue, nil) })
	waitForQueued(t, queue, 2, 1)
	launch("other inline", func() {
		handleChosenInlineResult(time.Now(), bot, nil, db, &tgbotapi.ChosenInlineResult{
			From: &tgbotapi.User{ID: 2}, ResultID: inlineResultID, InlineMessageID: "accepted", Query: "https://x.com/u/status/8",
		}, "@cache", queue, nil)
	})
	waitForQueued(t, queue, 2, 2)

	for _, path := range []string{"/u/status/4", "/u/status/5", "/u/status/7", "/u/status/8"} {
		api.permit <- struct{}{}
		if got := queueEvent(t, api.started); got != path {
			t.Fatalf("next request = %s, want %s", got, path)
		}
	}
	api.release()
	done := make(map[string]bool)
	for range 3 {
		done[queueEvent(t, finished)] = true
	}
	if !done["batch"] || !done["other chat"] || !done["other inline"] {
		t.Fatalf("completed: %v", done)
	}
	if got := api.maximum.Load(); got != 3 {
		t.Fatalf("maximum concurrent upstream requests = %d, want 3", got)
	}
	for _, id := range []int{1, 2, 3, 4, 5, 7, 8} {
		a, err := findArtwork(db, fmt.Sprintf("x:%d", id))
		if err != nil || a == nil {
			t.Fatalf("accepted artwork %d: %#v, %v", id, a, err)
		}
	}
	for _, id := range []int{6, 9, 10} {
		a, err := findArtwork(db, fmt.Sprintf("x:%d", id))
		if err != nil || a != nil {
			t.Fatalf("rejected artwork %d: %#v, %v", id, a, err)
		}
	}
}

func configuredMediaLimits(t *testing.T) mediaLimits {
	t.Helper()
	for _, name := range []string{"MEDIA_CONCURRENCY", "MEDIA_USER_QUEUE_LIMIT", "MEDIA_USER_HOURLY_LIMIT", "MEDIA_USER_DAILY_LIMIT"} {
		t.Setenv(name, "")
	}
	defaults, err := mediaLimitsFromEnv()
	if err != nil || defaults != (mediaLimits{concurrency: 3, queuePerUser: 20, perHour: 100, perDay: 600}) {
		t.Fatalf("default limits: %#v, %v", defaults, err)
	}
	t.Setenv("MEDIA_CONCURRENCY", "2")
	t.Setenv("MEDIA_USER_QUEUE_LIMIT", "8")
	t.Setenv("MEDIA_USER_HOURLY_LIMIT", "2")
	t.Setenv("MEDIA_USER_DAILY_LIMIT", "3")
	limits, err := mediaLimitsFromEnv()
	if err != nil || limits != (mediaLimits{concurrency: 2, queuePerUser: 8, perHour: 2, perDay: 3}) {
		t.Fatalf("configured limits: %#v, %v", limits, err)
	}
	t.Setenv("MEDIA_USER_DAILY_LIMIT", "0")
	if _, err := mediaLimitsFromEnv(); err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	t.Setenv("MEDIA_USER_DAILY_LIMIT", "3")
	return limits
}

func TestMediaQueueRollingLimits(t *testing.T) {
	limits := configuredMediaLimits(t)
	api, bot := newQueueTestAPI(t, false)
	api.failPath = "/u/status/1"
	db := testArtworkDB(t)
	queue := newMediaQueue(limits)

	// An upstream failure still consumes a link attempt.
	handleMessage(bot, nil, db, queuedChat(1, 1, 1), queue, nil)
	if message := queueEvent(t, api.feedback); !strings.Contains(message, "Could not process") {
		t.Fatalf("upstream failure: %s", message)
	}
	handleMessage(bot, nil, db, queuedChat(1, 2, 2), queue, nil)
	handleChosenInlineResult(time.Now(), bot, nil, db, &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, ResultID: inlineResultID, InlineMessageID: "limited", Query: "https://x.com/u/status/3",
	}, "@cache", queue, nil)
	if message := strings.ToLower(queueEvent(t, api.edits)); !strings.Contains(message, "hour") || !strings.Contains(message, "retry later") {
		t.Fatalf("hourly rejection: %s", message)
	}
	// Another user has an independent budget.
	handleMessage(bot, nil, db, queuedChat(2, 4, 4), queue, nil)

	queue.mu.Lock()
	for i := range queue.users[1].started {
		queue.users[1].started[i] = time.Now().Add(-2 * time.Hour)
	}
	queue.mu.Unlock()
	handleMessage(bot, nil, db, queuedChat(1, 5, 5), queue, nil)
	handleMessage(bot, nil, db, queuedChat(1, 6, 6), queue, nil)
	if message := strings.ToLower(queueEvent(t, api.feedback)); !strings.Contains(message, "day") && !strings.Contains(message, "daily") {
		t.Fatalf("daily rejection: %s", message)
	}
	queue.mu.Lock()
	queue.users[1].started[0] = time.Now().Add(-25 * time.Hour)
	queue.mu.Unlock()
	handleMessage(bot, nil, db, queuedChat(1, 7, 7), queue, nil)

	// Cached selections do not fetch media and remain available at the limit.
	handleChosenInlineResult(time.Now(), bot, nil, db, &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, ResultID: "x:2",
	}, "@cache", queue, nil)
	var inlineOrigin int
	if err := db.QueryRow(`SELECT inline_origin FROM history WHERE user_id = 1 AND artwork_id = 'x:2'`).Scan(&inlineOrigin); err != nil || inlineOrigin != 1 {
		t.Fatalf("cached selection: inline_origin = %d, %v", inlineOrigin, err)
	}
	api.mu.Lock()
	got := strings.Join(api.fetched, ",")
	api.mu.Unlock()
	if want := "/u/status/1,/u/status/2,/u/status/4,/u/status/5,/u/status/7"; got != want {
		t.Fatalf("upstream requests = %s, want %s", got, want)
	}
}

func TestMediaQueueReservesRateCapacity(t *testing.T) {
	api, bot := newQueueTestAPI(t, true)
	db := testArtworkDB(t)
	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 5, perHour: 2, perDay: 10})
	finished := make(chan string, 8)
	var wg sync.WaitGroup
	defer func() { api.release(); wg.Wait() }()
	launch := func(name string, id int) {
		wg.Go(func() { handleMessage(bot, nil, db, queuedChat(1, id, id), queue, nil); finished <- name })
	}

	launch("first", 1)
	if got := queueEvent(t, api.started); got != "/u/status/1" {
		t.Fatalf("first request: %s", got)
	}
	launch("waiting", 2)
	waitForQueued(t, queue, 1, 1)
	launch("rejected", 3)
	if message := strings.ToLower(queueEvent(t, api.feedback)); !strings.Contains(message, "hour") {
		t.Fatalf("queued job did not reserve hourly capacity: %s", message)
	}
	if got := queueEvent(t, finished); got != "rejected" {
		t.Fatalf("unexpected completion: %s", got)
	}

	// Expiring the start does not expire the still-waiting reservation.
	queue.mu.Lock()
	queue.users[1].started[0] = time.Now().Add(-2 * time.Hour)
	queue.mu.Unlock()
	launch("second waiting", 4)
	waitForQueued(t, queue, 1, 2)
	launch("rejected again", 5)
	if message := strings.ToLower(queueEvent(t, api.feedback)); !strings.Contains(message, "hour") {
		t.Fatalf("waiting reservations did not retain hourly capacity: %s", message)
	}
	if got := queueEvent(t, finished); got != "rejected again" {
		t.Fatalf("unexpected completion: %s", got)
	}
	api.release()
	for range 3 {
		queueEvent(t, finished)
	}
	for _, id := range []int{1, 2, 4} {
		a, err := findArtwork(db, fmt.Sprintf("x:%d", id))
		if err != nil || a == nil {
			t.Fatalf("accepted artwork %d: %#v, %v", id, a, err)
		}
	}
}
