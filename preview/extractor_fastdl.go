package preview

import (
	"context"
	"fmt"
	"net/url"
)

var _ Extractor = (*FastDLExtractor)(nil)

type FastDLExtractor struct {
	Endpoint string
}

func NewFastDL(config *url.URL) (*FastDLExtractor, error) {
	endpoint := *config
	endpoint.Scheme = "http"
	return &FastDLExtractor{
		Endpoint: endpoint.String(),
	}, nil
}

func (fdl *FastDLExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"instagram.com/reel/*",
		"instagram.com/p/*",
		"instagram.com/story/*",
	})
}

type VidProxyRequest struct {
	Target string `json:"target"`
}

type VidProxyResponse struct {
	RemoteURLs []string `json:"remote_urls"`
}

type VidProxyError struct {
	Message string `json:"msg"`
}

func (vpe VidProxyError) Error() string {
	return vpe.Message
}

func (fdl *FastDLExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	ctx, span := tracer.Start(ctx, "fastdl_extract")
	defer span.End()

	_, value, err := JSONRequest[VidProxyResponse, VidProxyError](ctx, "POST", fdl.Endpoint, VidProxyRequest{mediaURL})
	if err != nil {
		return nil, fmt.Errorf("making fastdl request: %w", err)
	}
	return value.RemoteURLs, nil
}
