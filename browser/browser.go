package browser

import (
	"fmt"
	"net/url"
	"os"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

var (
	browser *rod.Browser

	// cdpURL is set when connecting to a shared CDP endpoint (e.g. browserless).
	// Each page gets its own connection, since browserless treats every
	// connection as a separate, time-limited browser session.
	cdpURL string
)

func Start() (stop func() error) {
	if raw := os.Getenv("CDP_URL"); raw != "" {
		u, err := parseCDPURL(raw)
		if err != nil {
			panic(fmt.Sprintf("parsing CDP_URL: %v", err))
		}
		cdpURL = u
		// fail fast if the endpoint is unreachable
		b := rod.New().ControlURL(cdpURL).MustConnect()
		_ = b.Close()
		return func() error { return nil }
	} else if rodURL := os.Getenv("ROD_URL"); rodURL != "" {
		l := launcher.MustNewManaged(rodURL)
		browser = rod.New().Client(l.MustClient()).MustConnect()
	} else {
		path, found := launcher.LookPath()
		if !found {
			panic("no browser found")
		}
		u := launcher.New().
			Bin(path).
			MustLaunch()
		browser = rod.New().ControlURL(u).MustConnect()
	}

	return browser.Close
}

// parseCDPURL accepts a ws(s):// endpoint, or http(s):// which is converted to
// ws(s)://. The path and query (e.g. ?token=) are kept as-is.
func parseCDPURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("expecting ws://, wss://, http:// or https://: %s", raw)
	}
	return u.String(), nil
}

// newPage opens a page on the local/managed browser, or on a fresh connection
// to CDP_URL. cleanup closes the page, and the connection when using CDP_URL.
func newPage() (page *rod.Page, cleanup func() error, err error) {
	if cdpURL == "" {
		page, err = browser.Page(proto.TargetCreateTarget{})
		if err != nil {
			return nil, nil, err
		}
		return page, page.Close, nil
	}

	b := rod.New().ControlURL(cdpURL)
	if err = b.Connect(); err != nil {
		return nil, nil, fmt.Errorf("connecting to cdp: %w", err)
	}
	page, err = b.Page(proto.TargetCreateTarget{})
	if err != nil {
		_ = b.Close()
		return nil, nil, err
	}
	return page, b.Close, nil
}

func GetPage() (page *rod.Page, cleanup func() error) {
	page, cleanup, err := newPage()
	if err != nil {
		panic(err)
	}
	return page, cleanup
}

func GetHeader(headers []*proto.FetchHeaderEntry, name string) string {
	i := slices.IndexFunc(headers, func(e *proto.FetchHeaderEntry) bool {
		return e.Name == name
	})
	if i == -1 {
		return ""
	}
	return headers[i].Value
}

func Try[T any](fn func(page *rod.Page) T) (t T, err error) {
	page, cleanup, err := newPage()
	if err != nil {
		return t, fmt.Errorf("creating page in try: %w", err)
	}
	defer cleanup()

	defer func() {
		if val := recover(); val != nil {
			err = &rod.TryError{Value: val, Stack: string(debug.Stack())}
		}
	}()
	t = fn(page)

	return t, err
}

func TryChan[T any](fn func(page *rod.Page, ret chan T)) (t T, err error) {
	page, cleanup, err := newPage()
	if err != nil {
		return t, fmt.Errorf("creating page in trychan: %w", err)
	}
	defer cleanup()

	ret := make(chan T, 1)
	go func() {
		defer func() {
			if val := recover(); val != nil {
				err = &rod.TryError{Value: val, Stack: string(debug.Stack())}
			}
			close(ret)
		}()
		fn(page, ret)
	}()
	t = <-ret

	return t, err
}

func BlockThirdParty(page *rod.Page, firstPartyPattern string) {
	_ = proto.FetchEnable{
		Patterns: []*proto.FetchRequestPattern{
			{
				RequestStage: proto.FetchRequestStageRequest,
			},
		},
	}.Call(page)

	go page.EachEvent(func(e *proto.FetchRequestPaused) (stop bool) {
		// block third party requests
		if !strings.Contains(e.Request.URL, firstPartyPattern) {
			_ = proto.FetchFailRequest{
				RequestID:   e.RequestID,
				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
			}.Call(page)
			return false
		}

		// block unnescessary requests
		switch e.ResourceType {
		case proto.NetworkResourceTypeDocument,
			proto.NetworkResourceTypeScript,
			proto.NetworkResourceTypeXHR,
			proto.NetworkResourceTypeFetch,
			proto.NetworkResourceTypeOther:
			//fmt.Println("CONTINUE", e.Request.Method, e.Request.URL, e.ResourceType)
			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(page)
			return false
		default:
			//fmt.Println("BLOCK", e.Request.Method, e.Request.URL, e.ResourceType)
			_ = proto.FetchFailRequest{
				RequestID:   e.RequestID,
				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
			}.Call(page)
			return false
		}
	})()
}
