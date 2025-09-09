package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/match"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var (
	httpClient *http.Client
	cookieJar  *cookiejar.Jar
	userAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36"
)

func init() {
	cookieJar, _ = cookiejar.New(nil)

	httpClient = &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   30 * time.Second,
		Jar:       cookieJar,
	}
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	return httpDo(req)
}

func httpDo(req *http.Request) (*http.Response, error) {
	_, span := tracer.Start(req.Context(), "http_do", trace.WithAttributes(
		attribute.String("url", req.URL.String()),
		attribute.String("method", req.Method),
	))
	defer span.End()

	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	} else {
		span.SetStatus(codes.Ok, resp.Status)
		span.SetAttributes(
			attribute.Int64("content_length", resp.ContentLength),
			attribute.String("content_type", resp.Header.Get("Content-Type")),
		)
	}

	return resp, err
}

func getResponseExtension(resp *http.Response) string {
	// Check Content-Disposition first
	{
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			_, params, _ := mime.ParseMediaType(cd)
			if filename, ok := params["filename"]; ok {
				ext := filepath.Ext(filename)
				if ext != "" {
					return strings.ToLower(ext)
				}
			}
		}
	}

	// Check Content-Type
	{
		mediatype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		exts, err := mime.ExtensionsByType(mediatype)
		if err == nil && len(exts) > 0 {
			return strings.ToLower(exts[0])
		}
	}

	// Check extension in url
	{
		ext := filepath.Ext(resp.Request.URL.Path)
		if ext != "" {
			return strings.ToLower(ext)
		}
	}

	return ""
}

func simpleURLMatch(url string, patterns []string) bool {
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "www.")
	for _, p := range patterns {
		if ok := match.Match(url, p); ok {
			return true
		}
	}
	return false
}

func validateSimpleFilename(filename string) error {
	if filepath.Base(filename) != filename {
		return fmt.Errorf("filename %q must not contain path separators", filename)
	}
	return nil

}

func JSONRequest[V any, E error](ctx context.Context, method, url string, body any, headers ...string) (*http.Response, *V, error) {
	ctx, span := tracer.Start(ctx, "json_request")
	defer span.End()

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	for i := 0; i < len(headers); i += 2 {
		if headers[i+1] != "" {
			req.Header.Set(headers[i], headers[i+1])
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("sending http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("reading response body: %s: %w", resp.Status, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errorJSON E
		if err := json.Unmarshal(respBody, &errorJSON); err != nil {
			return resp, nil, fmt.Errorf("parsing error body: %s: %w", resp.Status, err)
		}
		return resp, nil, errorJSON // TODO: wrap with error code?
	}

	var valueJSON V
	if err := json.Unmarshal(respBody, &valueJSON); err != nil {
		return resp, nil, fmt.Errorf("parsing response: %s: %w", resp.Status, err)
	}
	return resp, &valueJSON, nil
}

func respReadAll(ctx context.Context, resp *http.Response) ([]byte, error) {
	_, span := tracer.Start(ctx, "resp_read_all", trace.WithAttributes(
		attribute.Int64("content_length", resp.ContentLength),
	))
	defer span.End()
	defer resp.Body.Close()

	if resp.ContentLength > 0 {
		buf := make([]byte, resp.ContentLength)
		_, err := io.ReadFull(resp.Body, buf)
		if err != nil {
			return nil, fmt.Errorf("reading full response body: %w", err)
		}
		return buf, nil
	}

	return nil, nil
}
