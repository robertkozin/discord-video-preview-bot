package preview

import (
	"net/url"
	"strings"
)

// ProbablyVideo reports whether a social media link could plausibly lead to a
// video. It only returns false when the link is definitely not one: a profile,
// a search or hashtag page, a photo post. Anything it can't tell (an Instagram
// /p/ post can be either) is true, so a true result is "worth trying" and not
// a promise. Unknown sites are true.
func ProbablyVideo(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	segs := pathSegments(u.Path)
	has := func(s string) bool {
		for _, seg := range segs {
			if seg == s {
				return true
			}
		}
		return false
	}

	switch {
	case host == "x.com" || host == "twitter.com" || host == "mobile.twitter.com":
		// only a status can hold media, and /photo/<n> points at a photo
		return len(segs) >= 3 && segs[1] == "status" && !has("photo")

	case host == "t.co" || host == "redd.it" || host == "v.redd.it" || host == "clips.twitch.tv" || host == "youtu.be":
		return len(segs) > 0

	case host == "tiktok.com" || strings.HasSuffix(host, ".tiktok.com"):
		if has("photo") {
			return false
		}
		if host != "tiktok.com" && host != "m.tiktok.com" && host != "pro.tiktok.com" {
			return len(segs) > 0 // vm. / vt. short links
		}
		// /@user/video/<id> and the short forms /t/<code>, /d/..., /v/<id>;
		// not profiles (/@user), /tag/, /music/, /discover, /search
		return has("video") || (len(segs) > 0 && (segs[0] == "t" || segs[0] == "d" || segs[0] == "v"))

	case host == "instagram.com":
		// reels, posts and shares; not profiles, /explore, /accounts or stories
		for _, s := range []string{"reel", "reels", "p", "tv", "share"} {
			if has(s) {
				return true
			}
		}
		return false

	case host == "youtube.com" || host == "m.youtube.com":
		return has("shorts") || has("watch") || has("live")

	case host == "twitch.tv":
		return has("clip") || has("videos")

	case host == "bsky.app":
		return len(segs) >= 4 && segs[0] == "profile" && segs[2] == "post"

	case host == "reddit.com" || host == "old.reddit.com" || host == "new.reddit.com":
		return len(segs) >= 4 && segs[0] == "r" && segs[2] == "comments"
	}

	return true
}

func pathSegments(path string) []string {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, strings.ToLower(s))
		}
	}
	return segs
}
