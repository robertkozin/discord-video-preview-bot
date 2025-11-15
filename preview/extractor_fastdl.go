package preview

import (
	"context"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/robertkozin/discord-video-preview-bot/browser"
)

var _ Extractor = (*FastDLExtractor)(nil)

type FastDLExtractor struct{}

func (fdl *FastDLExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"instagram.com/reel/*",
		"instagram.com/reels/*",
		"instagram.com/p/*",
		"instagram.com/story/*",
	})
}

func (ex *FastDLExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	ctx, span := tracer.Start(ctx, "fastdl_extract")
	defer span.End()

	remoteURL, err := browser.TryChan(func(page *rod.Page, ret chan string) {
		page = page.Timeout(15 * time.Second)
		ex.extract(page, mediaURL, ret)
	})
	return []string{remoteURL}, err
}

func (fdl *FastDLExtractor) extract(page *rod.Page, target string, ret chan string) {
	browser.BlockThirdParty(page, "fastdl.app")

	page.MustNavigate("https://fastdl.app/en").MustWaitLoad()

	page.MustElement("input").MustClick().MustInput(target).MustType(input.Enter)

	page.Race().Element("a.button__download").MustHandle(func(e *rod.Element) {
		ret <- e.MustProperty("href").Str()
	}).Element("div.error-message").MustHandle(func(e *rod.Element) {
		panic(e.MustText())
	}).MustDo()
}

// type InterceptResult struct {
// 	StatusCode  int
// 	ContentType string
// 	Body        []byte
// }

// func BlockThirdParty(page *rod.Page, firstPartyPattern string) {
// 	_ = proto.FetchEnable{
// 		Patterns: []*proto.FetchRequestPattern{
// 			{
// 				RequestStage: proto.FetchRequestStageRequest,
// 			},
// 		},
// 	}.Call(page)

// 	go page.EachEvent(func(e *proto.FetchRequestPaused) (stop bool) {
// 		// block third party requests
// 		if !strings.Contains(e.Request.URL, firstPartyPattern) {
// 			_ = proto.FetchFailRequest{
// 				RequestID:   e.RequestID,
// 				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
// 			}.Call(page)
// 			return false
// 		}

// 		// block unnescessary requests
// 		switch e.ResourceType {
// 		case proto.NetworkResourceTypeDocument,
// 			proto.NetworkResourceTypeScript,
// 			proto.NetworkResourceTypeXHR,
// 			proto.NetworkResourceTypeFetch,
// 			proto.NetworkResourceTypeOther:
// 			//fmt.Println("continue", e.Request.Method, e.Request.URL, e.ResourceType)
// 			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(page)
// 			return false
// 		default:
// 			//fmt.Println("block", e.Request.Method, e.Request.URL, e.ResourceType)
// 			_ = proto.FetchFailRequest{
// 				RequestID:   e.RequestID,
// 				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
// 			}.Call(page)
// 			return false
// 		}
// 	})()
// }

// func BlockThirdPartyAndInterceptOnce(
// 	page *rod.Page, firstPartyPattern string, interceptPattern string,
// ) chan InterceptResult {
// 	_ = proto.FetchEnable{
// 		Patterns: []*proto.FetchRequestPattern{
// 			{
// 				RequestStage: proto.FetchRequestStageRequest,
// 			},
// 			{
// 				URLPattern:   interceptPattern,
// 				RequestStage: proto.FetchRequestStageResponse,
// 			},
// 		},
// 	}.Call(page)

// 	ch := make(chan InterceptResult, 1)

// 	go page.EachEvent(func(e *proto.FetchRequestPaused) (stop bool) {
// 		// intercept targeted response
// 		if e.ResponseErrorReason != "" {

// 			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(page)
// 			_ = proto.FetchDisable{}.Call(page)
// 			return true
// 		} else if e.ResponseStatusCode != nil {
// 			contentType := browser.GetHeader(e.ResponseHeaders, "Content-Type")

// 			result, _ := proto.FetchGetResponseBody{RequestID: e.RequestID}.Call(page)
// 			var body []byte
// 			if result.Base64Encoded {
// 				body, _ := base64.StdEncoding.DecodeString(result.Body)
// 				ch <- InterceptResult{*e.ResponseStatusCode, b}
// 			} else {
// 				body = []byte(result.Body)
// 				ch <- InterceptResult{*e.ResponseStatusCode}
// 			}

// 			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(page)
// 			_ = proto.FetchDisable{}.Call(page)
// 			return true
// 		}

// 		// block third party requests
// 		if !strings.Contains(e.Request.URL, firstPartyPattern) {
// 			_ = proto.FetchFailRequest{
// 				RequestID:   e.RequestID,
// 				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
// 			}.Call(page)
// 			return false
// 		}

// 		// block unnescessary requests
// 		switch e.ResourceType {
// 		case proto.NetworkResourceTypeDocument,
// 			proto.NetworkResourceTypeScript,
// 			proto.NetworkResourceTypeXHR,
// 			proto.NetworkResourceTypeFetch,
// 			proto.NetworkResourceTypeOther:
// 			//fmt.Println("continue", e.Request.Method, e.Request.URL, e.ResourceType)
// 			_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(page)
// 			return false
// 		default:
// 			//fmt.Println("block", e.Request.Method, e.Request.URL, e.ResourceType)
// 			_ = proto.FetchFailRequest{
// 				RequestID:   e.RequestID,
// 				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
// 			}.Call(page)
// 			return false
// 		}
// 	})()

// 	return ch
// }

// func ExtractFastDl2(ctx context.Context, page *rod.Page, target string) ([]string, error) {
// 	page = page.Timeout(time.Second * 15)

// 	resultCh := BlockThirdPartyAndInterceptOnce(page, "fastdl.app", "*/api/convert*")

// 	_ = rod.Try(func() {
// 		page.MustNavigate("https://fastdl.app/en").MustWaitLoad()
// 		page.MustElement("input").MustClick().MustInput(target).MustType(input.Enter)
// 	})

// 	select {
// 	case result := <-resultCh:
// 		fmt.Println("RESULT", result.StatusCode, gjson.ParseBytes(result.Body))
// 	case <-page.GetContext().Done():
// 		fmt.Println("TIMEOUT")
// 	}

// 	return nil, nil
// }

// func processResult(result InterceptResult) ([]string, error) {
// 	if result.ContentType != "application/json" {
// 		return nil, fmt.Errorf("expecting application/json header: %s", result.ContentType)
// 	}

// 	body := gjson.ParseBytes(result.Body)

// 	if result.StatusCode != 200 {

// 	}

// }
