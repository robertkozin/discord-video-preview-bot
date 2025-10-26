package preview

import (
	"context"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/robertkozin/discord-video-preview-bot/browser"
)

var _ Extractor = (*SSSTikExtractor)(nil)

type SSSTikExtractor struct{}

func (ex *SSSTikExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"tiktok.com/t/*",
		"tiktok.com/@*/video/*",
		"vm.tiktok.com/*",
	})
}

func (ex *SSSTikExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	ctx, span := tracer.Start(ctx, "ssstik_extract")
	defer span.End()

	remoteURL, err := browser.TryChan(func(page *rod.Page, ret chan string) {
		ex.extract(page, mediaURL, ret)
	})
	return []string{remoteURL}, err
}

func (ex *SSSTikExtractor) extract(page *rod.Page, target string, ret chan string) {
	browser.BlockThirdParty(page, "ssstik.io")

	page.MustNavigate("https://ssstik.io/").MustWaitLoad()

	page.MustElement("input[autofocus]").MustClick().MustInput(target).MustType(input.Enter)

	page.Race().Element("a.without_watermark").MustHandle(func(e *rod.Element) {
		ret <- e.MustProperty("href").Str()
	}).Element("div.critical").MustHandle(func(e *rod.Element) {
		panic(e.MustText())
	}).MustDo()
}
