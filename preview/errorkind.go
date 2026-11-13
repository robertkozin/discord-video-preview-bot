package preview

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// Error kinds, the fixed values of the error.kind span attribute, so a
// failure class can be counted without searching free-text messages.
const (
	kindLoginRequired = "login_required" // a session was rejected or the post needs an account
	kindGeoblock      = "geoblock"       // region lock (TikTok 10231, Instagram geoblock_required, YouTube UNPLAYABLE)
	kindBotCheck      = "bot_check"      // "confirm you're not a bot", challenges
	kindPrivate       = "private"        // private or restricted post
	kindRateLimited   = "rate_limited"
	kindTimeout       = "timeout"
	kindNoMedia       = "no_media" // the post loaded but nothing usable was in it
	kindHTTP          = "http_error"
	kindOther         = "other"
)

var (
	geoblockRe = regexp.MustCompile(`geoblock|cross_border|\b10231\b|UNPLAYABLE|not available in your`)
	loginRe    = regexp.MustCompile(`login_required|login required|LOGIN_REQUIRED|session rejected|checkpoint|without login|needs a login|redirected to login|require_login`)
	botRe      = regexp.MustCompile(`(?i)not a bot|bot check|challenge|captcha`)
	privateRe  = regexp.MustCompile(`(?i)\bprivate\b|\b10216\b|\b10222\b|status_self_see`)
	rateRe     = regexp.MustCompile(`\b429\b|(?i)too many requests|rate limit`)
	timeoutRe  = regexp.MustCompile(`(?i)timed out|timeout|deadline exceeded|stalled`)
	noMediaRe  = regexp.MustCompile(`(?i)no media|no video|no h264|card but no media|no post data|no jpeg|no playable|has no hosted video|not found|no usable`)
	httpRe     = regexp.MustCompile(`\b[45]\d\d\b`)
)

// errorKind classifies err into one of the kind* values. It matches on the
// messages the extractors and the sites produce, so it is a best guess meant
// for filtering traces, not for control flow. A bot check is tested before the
// login cases because YouTube words it as LOGIN_REQUIRED.
func errorKind(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errDownloadStalled) {
		return kindTimeout
	}
	msg := err.Error()
	switch {
	case geoblockRe.MatchString(msg):
		return kindGeoblock
	case botRe.MatchString(msg):
		return kindBotCheck
	case loginRe.MatchString(msg):
		return kindLoginRequired
	case privateRe.MatchString(msg):
		return kindPrivate
	case rateRe.MatchString(msg):
		return kindRateLimited
	case timeoutRe.MatchString(msg):
		return kindTimeout
	case noMediaRe.MatchString(msg):
		return kindNoMedia
	case httpRe.MatchString(strings.ToLower(msg)):
		return kindHTTP
	}
	return kindOther
}
