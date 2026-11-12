package preview

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// shortLinkClient doesn't follow redirects, so a short link's target can be
// read from its Location header.
var shortLinkClient = &http.Client{
	Transport:     http.DefaultTransport,
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// isTikTokShortLink reports whether rawURL is a TikTok share link that only
// redirects somewhere: vm.tiktok.com/<code>, vt.tiktok.com/<code> or
// (www|m|pro.)tiktok.com/t/<code>.
func isTikTokShortLink(u *url.URL) bool {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	segs := pathSegments(u.Path)
	switch host {
	case "vm.tiktok.com", "vt.tiktok.com":
		return len(segs) > 0
	case "tiktok.com", "m.tiktok.com", "pro.tiktok.com":
		return len(segs) >= 2 && segs[0] == "t"
	}
	return false
}

// tikTokNonVideo reports whether a TikTok url is a page that is never a video:
// a TikTok Shop product or a referral/rewards campaign, which share links
// often point at.
func tikTokNonVideo(u *url.URL) bool {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "tiktok.com" && !strings.HasSuffix(host, ".tiktok.com") {
		return false
	}
	segs := pathSegments(u.Path)
	if len(segs) == 0 {
		return false // the home page can also be a block or region redirect
	}
	switch segs[0] {
	case "view", "ug", "shop", "product", "coupon":
		return true
	}
	return false
}

// resolveTikTokShortLink follows a TikTok share link's redirects to the page it
// stands for, so the extractors get a /@user/video/<id> url (tikwm and cobalt
// can't resolve some short links themselves). A link to a shop or referral page
// is ErrNoVideo. Any other url, or a short link that can't be resolved, is
// returned unchanged for the extractors to try.
func resolveTikTokShortLink(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !isTikTokShortLink(u) {
		return rawURL, nil
	}

	ctx, span := tracer.Start(ctx, "resolve_short_link")
	defer EndSpan(span, &err)
	span.SetAttributes(attribute.String("short_link", rawURL))

	current := u
	for hop := 0; hop < 3 && isTikTokShortLink(current); hop++ {
		next, ok := redirectTarget(ctx, current)
		if !ok {
			span.SetAttributes(attribute.Bool("short_link.resolved", false))
			return rawURL, nil
		}
		current = next
	}
	span.SetAttributes(attribute.Bool("short_link.resolved", true), attribute.String("short_link.target", truncate(current.String(), 300)))

	if tikTokNonVideo(current) {
		err = fmt.Errorf("tiktok short link points to a non-video page (%s): %w", current.Path, ErrNoVideo)
		return "", err
	}
	if isTikTokShortLink(current) {
		return rawURL, nil // still a short link after a few hops
	}
	return current.String(), nil
}

// redirectTarget asks for u and returns where it redirects to.
func redirectTarget(ctx context.Context, u *url.URL) (*url.URL, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false
	}
	resp, err := httpDoWith(shortLinkClient, req)
	if err != nil {
		return nil, false
	}
	resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return nil, false
	}
	next, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
	if err != nil || next.Host == "" {
		return nil, false
	}
	return next, true
}
