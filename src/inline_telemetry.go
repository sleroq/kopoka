package main

import (
	"context"
	"errors"
	"os"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const inlineInstrumentation = "kopoka/inline"

var inlineLatency, _ = otel.Meter(inlineInstrumentation).Float64Histogram("kopoka.inline.duration",
	metric.WithUnit("s"),
	metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.35, 0.5, 0.75, 1, 1.5, 2, 3, 5, 8, 15, 30, 60, 120),
)

func startInlineTelemetry(ctx context.Context) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return nil, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "kopoka")),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, err
	}
	traces, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	metrics, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, errors.Join(err, traces.Shutdown(ctx))
	}
	// The collector makes the sampling decision after seeing errors and latency.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res), sdktrace.WithBatcher(traces), sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics)),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	return func(ctx context.Context) error { return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)) }, nil
}

func startInlineRoot(ctx context.Context, name string, started time.Time) (context.Context, trace.Span) {
	return otel.Tracer(inlineInstrumentation).Start(ctx, name, trace.WithNewRoot(), trace.WithTimestamp(started))
}

func startInlineSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if parent := trace.SpanFromContext(ctx); parent.IsRecording() {
		return parent.TracerProvider().Tracer(inlineInstrumentation).Start(ctx, name, trace.WithAttributes(attrs...))
	}
	return ctx, trace.SpanFromContext(ctx)
}

// Shared work can outlive the answer and can be awaited by unrelated queries.
func startInlineBackground(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if parent := trace.SpanFromContext(ctx); parent.IsRecording() {
		return parent.TracerProvider().Tracer(inlineInstrumentation).Start(context.Background(), name,
			trace.WithLinks(trace.LinkFromContext(ctx)), trace.WithAttributes(attrs...))
	}
	return context.Background(), trace.SpanFromContext(context.Background())
}

func inlineDispatch(ctx context.Context, received time.Time) {
	if parent := trace.SpanFromContext(ctx); parent.IsRecording() {
		tracer := parent.TracerProvider().Tracer(inlineInstrumentation)
		_, span := tracer.Start(ctx, "inline.dispatch", trace.WithTimestamp(received))
		span.End()
	}
}

func inlineSpanError(span trace.Span, err error) {
	if err == nil || errors.Is(err, errNoXMedia) || !span.IsRecording() {
		return
	}
	kind := "error"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, errXPostNotFound):
		kind = "not_found"
	default:
		if telegram, ok := errors.AsType[*tgbotapi.Error](err); ok {
			kind = "telegram"
			span.SetAttributes(attribute.Int("telegram.error_code", telegram.Code))
		} else if upstream, ok := errors.AsType[upstreamHTTPError](err); ok {
			kind = "upstream_http"
			span.SetAttributes(attribute.Int("http.response.status_code", upstream.status))
		}
	}
	// Error messages from dependencies can contain URLs, credentials, or user input.
	span.SetStatus(codes.Error, kind)
}

func recordInlineLatency(ctx context.Context, received time.Time, operation, route, cache, outcome string) {
	inlineLatency.Record(ctx, time.Since(received).Seconds(), metric.WithAttributes(
		attribute.String("inline.operation", operation), attribute.String("inline.route", route),
		attribute.String("inline.cache", cache), attribute.String("inline.outcome", outcome),
	))
}
