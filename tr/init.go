package tr

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

var tp trace.TracerProvider

func init() {
	var err error
	tp, err = initTracer("discord-video-preview-bot")
	if err != nil {
		panic("initializing tracer: " + err.Error())
	}
}

func Shutdown() {
	if tp == nil {
		return
	}

	if sdk, ok := tp.(*sdktrace.TracerProvider); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sdk.Shutdown(ctx)
	}
}

func initTracer(serviceName string) (trace.TracerProvider, error) {
	ctx := context.Background()

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return noop.NewTracerProvider(), nil
	}

	// Create resources
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating otel resource: %w", err)
	}

	// Create exporter
	opts := []otlptracegrpc.Option{}
	opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))

	isLocal, err := isLoopbackAddress(endpoint)
	if err != nil {
		return nil, fmt.Errorf("figuring out if %q is a local address: %w", endpoint, err)
	} else if isLocal {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	rawHeaders, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_HEADERS")
	if ok && rawHeaders != "" {
		headers := parseOtelEnvHeaders(rawHeaders)
		opts = append(opts, otlptracegrpc.WithHeaders(headers))
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating otlp trace grpc exporter: %w", err)
	}

	// Create tracer provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)

	otel.SetTracerProvider(tp)

	return tp, nil
}

func parseOtelEnvHeaders(fromEnv string) map[string]string {
	headers := map[string]string{}
	for _, pair := range strings.Split(fromEnv, ",") {
		key, val, _ := strings.Cut(pair, "=")
		headers[key] = val
	}
	return headers
}

func isLoopbackAddress(endpoint string) (bool, error) {
	hpRe := regexp.MustCompile(`^[\w.-]+:\d+$`)
	uriRe := regexp.MustCompile(`^(http|https)`)

	endpoint = strings.TrimSpace(endpoint)

	var hostname string
	if hpRe.MatchString(endpoint) {
		parts := strings.SplitN(endpoint, ":", 2)
		hostname = parts[0]
	} else if uriRe.MatchString(endpoint) {
		u, err := url.Parse(endpoint)
		if err != nil {
			return false, err
		}
		hostname = u.Hostname()
	}

	ips, err := net.LookupIP(hostname)
	if err != nil {
		return false, err
	}

	allAreLoopback := true
	for _, ip := range ips {
		if !ip.IsLoopback() && !ip.IsPrivate() {
			allAreLoopback = false
		}
	}

	return allAreLoopback, nil
}
