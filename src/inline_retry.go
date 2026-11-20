package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	inlinePreviewBudget  = 3 * time.Second
	inlineAnswerBudget   = 8 * time.Second
	inlineDeliveryBudget = 2 * time.Minute
)

type upstreamHTTPError struct {
	operation string
	status    int
}

func (e upstreamHTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d %s", e.operation, e.status, http.StatusText(e.status))
}

type inlineRetry struct {
	until   time.Time
	waiting func()
	span    trace.Span
}

func newInlineDelivery(ctx context.Context, bot *tgbotapi.BotAPI, messageID string) *inlineRetry {
	return &inlineRetry{until: time.Now().Add(inlineDeliveryBudget), span: trace.SpanFromContext(ctx), waiting: func() {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := editInlineButton(telegramWithContext(bot, ctx), messageID, "Still working…"); err != nil {
			log.Printf("updating inline retry status: %v", err)
		}
	}}
}

func retryInline[T any](r *inlineRetry, operation func() (T, error)) (T, error) {
	for backoff := 250 * time.Millisecond; ; backoff = min(backoff*2, 8*time.Second) {
		result, err := operation()
		if r == nil || err == nil {
			return result, err
		}
		delay, retry := inlineRetryDelay(err, backoff)
		if !retry || delay >= time.Until(r.until) {
			return result, err
		}
		if r.waiting != nil {
			waiting := r.waiting
			r.waiting = nil
			waiting()
		}
		if delay >= time.Until(r.until) {
			return result, err
		}
		if r.span != nil && r.span.IsRecording() {
			r.span.AddEvent("retry_backoff", trace.WithAttributes(attribute.Int64("retry.wait_ms", delay.Milliseconds())))
		}
		time.Sleep(delay)
	}
}

func inlineRetryDelay(err error, backoff time.Duration) (time.Duration, bool) {
	if errors.Is(err, errXPostNotFound) {
		return 0, false
	}
	if _, ok := errors.AsType[unavailablePhotoError](err); ok {
		return 0, false
	}
	if telegram, ok := errors.AsType[*tgbotapi.Error](err); ok {
		if telegram.RetryAfter > 0 && (telegram.Code == http.StatusTooManyRequests || telegram.Code == 0) {
			return max(backoff, time.Duration(telegram.RetryAfter)*time.Second), true
		}
		return backoff, transientHTTPStatus(telegram.Code)
	}
	if upstream, ok := errors.AsType[upstreamHTTPError](err); ok {
		return backoff, transientHTTPStatus(upstream.status)
	}
	if network, ok := errors.AsType[net.Error](err); ok && network.Timeout() {
		return backoff, true
	}
	_, networkFailure := errors.AsType[*net.OpError](err)
	return backoff, networkFailure || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func transientHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status < 600
}

func retryInlineTelegram[T any](r *inlineRetry, operation func() (T, error)) (T, error) {
	if r == nil {
		return retryTelegram(operation)
	}
	return retryInline(r, operation)
}

func editInlineMessage(bot *tgbotapi.BotAPI, r *inlineRetry, method string, params tgbotapi.Params) error {
	_, err := retryInlineTelegram(r, func() (*tgbotapi.APIResponse, error) { return bot.MakeRequest(method, params) })
	if telegram, ok := errors.AsType[*tgbotapi.Error](err); ok && strings.Contains(telegram.Message, "message is not modified") {
		return nil
	}
	return err
}

func editInlineButton(bot *tgbotapi.BotAPI, messageID, label string) error {
	params := tgbotapi.Params{
		"inline_message_id": messageID,
		"reply_markup":      fmt.Sprintf(`{"inline_keyboard":[[{"text":%q,"disabled":{}}]]}`, label),
	}
	_, err := bot.MakeRequest("editMessageReplyMarkup", params)
	return err
}

// The Telegram library doesn't attach request contexts or expose HTTP status errors.
type inlineTelegramClient struct {
	client tgbotapi.HTTPClient
	ctx    context.Context
}

func telegramWithContext(bot *tgbotapi.BotAPI, ctx context.Context) *tgbotapi.BotAPI {
	scoped := *bot
	client := bot.Client
	if scopedClient, ok := client.(inlineTelegramClient); ok {
		client = scopedClient.client
	}
	scoped.Client = inlineTelegramClient{client: client, ctx: ctx}
	return &scoped
}

func (c inlineTelegramClient) Do(req *http.Request) (*http.Response, error) {
	ctx, span := startInlineSpan(c.ctx, "telegram.request")
	defer span.End()
	if span.IsRecording() {
		method := path.Base(req.URL.Path)
		switch method {
		case "answerInlineQuery", "editMessageText", "editMessageMedia", "editMessageReplyMarkup",
			"sendMessage", "sendPhoto", "sendMediaGroup", "sendAnimation", "sendVideo", "deleteMessage", "deleteMessages":
			span.SetAttributes(attribute.String("telegram.method", method))
		}
	}
	res, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		inlineSpanError(span, err)
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", res.StatusCode))
	if res.StatusCode >= 400 {
		span.SetStatus(codes.Error, "telegram_http")
	}
	if res.StatusCode >= 500 && res.StatusCode < 600 {
		_ = res.Body.Close()
		return nil, upstreamHTTPError{operation: "Telegram", status: res.StatusCode}
	}
	return res, err
}
