package browser

import (
	"fmt"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

var browser *rod.Browser

func Start() (stop func() error) {
	path, found := launcher.LookPath()
	if !found {
		panic("no browser found")
	}
	debugURL := launcher.New().Bin(path).MustLaunch()
	browser = rod.New().ControlURL(debugURL).MustConnect()
	return browser.Close
}

func GetPage() (page *rod.Page, cleanup func() error) {
	page = browser.MustPage()
	return page, page.Close
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
	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return t, fmt.Errorf("creating page in try: %w", err)
	}
	defer page.Close()

	defer func() {
		if val := recover(); val != nil {
			err = &rod.TryError{Value: val, Stack: string(debug.Stack())}
		}
	}()
	t = fn(page)

	return t, err
}

func TryChan[T any](fn func(page *rod.Page, ret chan T)) (t T, err error) {
	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return t, fmt.Errorf("creating page in trychan: %w", err)
	}
	defer page.Close()

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
			fmt.Println("CONTINUE", e.Request.Method, e.Request.URL, e.ResourceType)
			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(page)
			return false
		default:
			fmt.Println("BLOCK", e.Request.Method, e.Request.URL, e.ResourceType)
			_ = proto.FetchFailRequest{
				RequestID:   e.RequestID,
				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
			}.Call(page)
			return false
		}
	})()
}
