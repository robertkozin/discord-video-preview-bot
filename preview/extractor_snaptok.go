package preview

import (
	"context"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/robertkozin/discord-video-preview-bot/browser"
)

var _ Extractor = (*SnapTokExtractor)(nil)

type SnapTokExtractor struct{}

func (ex *SnapTokExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"tiktok.com/t/*",
		"tiktok.com/@*/video/*",
		"*.tiktok.com/*",
	})
}

func (ex *SnapTokExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	ctx, span := tracer.Start(ctx, "snaptok_extract")
	defer span.End()

	remoteURL, err := browser.TryChan(func(page *rod.Page, ret chan string) {
		page = page.Timeout(15 * time.Second)
		ex.extract(page, mediaURL, ret)
	})
	return []string{remoteURL}, err
}

func (ex *SnapTokExtractor) extract(page *rod.Page, target string, ret chan string) {
	browser.BlockThirdParty(page, "snap-tok.com")

	page.MustNavigate("https://snap-tok.com/").MustWaitLoad()

	page.MustElement("input").MustClick().MustInput(target).MustType(input.Enter)

	page.Race().Element("a.download-btn").MustHandle(func(e *rod.Element) {
		ret <- e.MustProperty("href").Str()
	}).Element("div.error-message").MustHandle(func(e *rod.Element) {
		panic(e.MustText())
	}).MustDo()
}
