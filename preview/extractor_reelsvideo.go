package preview

import (
	"context"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/robertkozin/discord-video-preview-bot/browser"
)

var _ Extractor = (*ReelsVideoExtractor)(nil)

type ReelsVideoExtractor struct{}

func (ex *ReelsVideoExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"instagram.com/reel/*",
		"instagram.com/reels/*",
		"instagram.com/p/*",
		"instagram.com/story/*",
	})
}

func (ex *ReelsVideoExtractor) Extract(ctx context.Context, mediaURL string) (urls []string, err error) {
	_, span := tracer.Start(ctx, "reelsvideo_extract")
	defer EndExtractor(span, &err)

	remoteURL, err := browser.TryChan(func(page *rod.Page, ret chan string) {
		page = page.Timeout(15 * time.Second)
		ex.extract(page, mediaURL, ret)
	})
	if err != nil {
		return nil, err
	}
	return []string{remoteURL}, nil
}

func (ex *ReelsVideoExtractor) extract(page *rod.Page, target string, ret chan string) {
	browser.BlockThirdParty(page, "reelsvideo.io")

	page.MustNavigate("https://reelsvideo.io/").MustWaitLoad()

	page.MustElement("input[autofocus]").MustClick().MustInput(target).MustType(input.Enter)

	page.Race().Element("a.download_link").MustHandle(func(e *rod.Element) {
		ret <- e.MustProperty("href").Str()
	}).Element("#errorContainer > *").MustHandle(func(e *rod.Element) {
		panic(e.MustText())
	}).MustDo()
}
