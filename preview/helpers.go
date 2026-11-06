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
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"github.com/tidwall/match"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var (
	httpClient *http.Client
	// mediaClient downloads media. It has no total timeout, since a big file
	// over a slow link (a mobile proxy) legitimately takes a while; downloadHttp
	// ends a download that stops making progress instead.
	mediaClient *http.Client
	cookieJar   *cookiejar.Jar
	userAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36"
)

func init() {
	cookieJar, _ = cookiejar.New(nil)

	httpClient = &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   30 * time.Second,
		Jar:       cookieJar,
	}

	mediaTransport := http.DefaultTransport.(*http.Transport).Clone()
	mediaTransport.ResponseHeaderTimeout = 30 * time.Second
	mediaClient = &http.Client{Transport: mediaTransport, Jar: cookieJar}
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	return httpDo(req)
}

// httpGetMedia is httpGet for downloading media, see mediaClient.
func httpGetMedia(ctx context.Context, url string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	return httpDoWith(mediaClient, req)
}

func httpDo(req *http.Request) (*http.Response, error) {
	return httpDoWith(httpClient, req)
}

func httpDoWith(client *http.Client, req *http.Request) (*http.Response, error) {
	_, span := tracer.Start(req.Context(), "http_do", trace.WithAttributes(
		attribute.String("url", redactURL(req.URL)),
		attribute.String("method", req.Method),
	))
	defer span.End()

	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	} else {
		if resp.StatusCode >= 500 {
			span.SetStatus(codes.Error, resp.Status)
		} else {
			span.SetStatus(codes.Ok, resp.Status)
		}
		span.SetAttributes(
			attribute.String("status", resp.Status),
			attribute.Int("status_code", resp.StatusCode),
			attribute.Int64("content_length", resp.ContentLength),
			attribute.String("content_type", resp.Header.Get("Content-Type")),
			attribute.String("user_agent", resp.Request.UserAgent()),
		)
	}

	return resp, err
}

// traceSpan is the span in ctx, for setting attributes from a helper.
func traceSpan(ctx context.Context) trace.Span { return trace.SpanFromContext(ctx) }

// redactURL drops the query, fragment and credentials from u so signed urls
// (cobalt tunnels, CDN tokens) don't end up in traces.
func redactURL(u *url.URL) string {
	r := *u
	r.RawQuery, r.Fragment, r.User = "", "", nil
	return r.String()
}

// redactRawURL is redactURL for strings; file:// and unparsable values pass through.
func redactRawURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "unparsable"
	}
	return redactURL(u)
}

// siteOf returns a low-cardinality site name for a media url, e.g. "x.com",
// "instagram.com", "reddit.com".
func siteOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "unknown"
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "t.co", "twitter.com", "mobile.twitter.com":
		return "x.com"
	case "redd.it", "v.redd.it":
		return "reddit.com"
	case "youtu.be":
		return "youtube.com"
	}
	parts := strings.Split(host, ".")
	if len(parts) > 2 {
		host = strings.Join(parts[len(parts)-2:], ".")
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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

func JSONRequest[V any, E error](ctx context.Context, method, url string, body any, headers ...string) (resp *http.Response, value *V, err error) {
	ctx, span := tracer.Start(ctx, "json_request")
	defer tr.End(span, &err)

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
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

	resp, err = httpDo(req)
	if err != nil {
		return nil, nil, fmt.Errorf("sending http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("reading response body: %s: %w", resp.Status, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		span.SetAttributes(
			attribute.Int("http.status_code", resp.StatusCode),
			attribute.String("http.response.body", truncate(string(respBody), 512)),
		)
		var errorJSON E
		if err := json.Unmarshal(respBody, &errorJSON); err != nil {
			return resp, nil, fmt.Errorf("parsing error body: %s: %s: %w", resp.Status, truncate(string(respBody), 200), err)
		}
		return resp, nil, fmt.Errorf("%s: %w", resp.Status, errorJSON)
	}

	var valueJSON V
	if err := json.Unmarshal(respBody, &valueJSON); err != nil {
		return resp, nil, fmt.Errorf("parsing response: %s: %w", resp.Status, err)
	}
	return resp, &valueJSON, nil
}

// detectContentType is http.DetectContentType, except it recognizes mp4s whose
// ftyp box lacks an "mp4*" brand (e.g. X's "isom"/"iso4" files).
func detectContentType(body []byte) string {
	if len(body) >= 12 && string(body[4:8]) == "ftyp" {
		switch string(body[8:12]) {
		case "M4A ", "M4B ", "M4P ", "qt  ":
		default:
			return "video/mp4"
		}
	}
	return http.DetectContentType(body)
}

// SiteOf is siteOf for other packages.
func SiteOf(rawURL string) string { return siteOf(rawURL) }
