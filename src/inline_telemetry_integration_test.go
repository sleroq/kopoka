package main

import (
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestInlineQueryAndDeliveryTraces(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	f, previews, bot := newMessagePreviewFixture(t)
	const link = "https://x.com/user/status/2"
	query := link + " private-query-suffix"
	result := previewResult(t, f.query(t, previews, bot, query), "post-image:light:2")
	servedMessagePreview(t, previews, result, "/post-image-preview")

	queue := newMediaQueue(mediaLimits{concurrency: 1, queuePerUser: 20, perHour: 100, perDay: 600})
	chosen := &tgbotapi.ChosenInlineResult{
		From: &tgbotapi.User{ID: 1}, ResultID: "post-image:light:2",
		InlineMessageID: "private-message-id", Query: query,
	}
	handleChosenInlineResult(time.Now(), bot, nil, previews.db, chosen, "@cache", queue, previews)
	select {
	case <-f.edits:
	default:
		t.Fatal("no final message edit")
	}

	spans := recorder.Ended()
	search := requireInlineTraceSpan(t, spans, "inline.query")
	delivery := requireInlineTraceSpan(t, spans, "inline.chosen")
	for name, root := range map[string]sdktrace.ReadOnlySpan{
		"inline.answer": search, "inline.delivery_queue": delivery, "post_image.materialize": delivery,
	} {
		span := requireInlineTraceSpan(t, spans, name)
		if span.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("%s is not in the %s trace", name, root.Name())
		}
	}
	requireInlineTraceSpan(t, spans, "preview.message.render")
	requireInlineTraceSpan(t, spans, "preview.post_image.http")

	calls := map[string]bool{}
	for _, span := range spans {
		for _, kv := range span.Attributes() {
			if kv.Key == attribute.Key("telegram.method") {
				calls[kv.Value.AsString()] = true
			}
			for _, private := range []string{link, "private-query-suffix", "private-message-id", "bottoken"} {
				if strings.Contains(kv.Value.AsString(), private) {
					t.Errorf("private input in %s attribute %s", span.Name(), kv.Key)
				}
			}
		}
	}
	for _, method := range []string{"answerInlineQuery", "sendPhoto", "editMessageMedia"} {
		if !calls[method] {
			t.Errorf("missing Telegram span for %s", method)
		}
	}
}

func requireInlineTraceSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("missing span %s", name)
	return nil
}
