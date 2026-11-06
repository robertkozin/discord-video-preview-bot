package preview

import "testing"

func TestProbablyVideo(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		// x
		{"https://x.com/user/status/123", true},
		{"https://x.com/user/status/123/video/1", true},
		{"https://x.com/user/status/123/photo/1", false},
		{"https://x.com/user", false},
		{"https://x.com/search?q=cats", false},
		{"https://twitter.com/user/status/123", true},
		// tiktok
		{"https://www.tiktok.com/@user/video/123", true},
		{"https://www.tiktok.com/@user/photo/123", false},
		{"https://www.tiktok.com/@user", false},
		{"https://www.tiktok.com/tag/cats", false},
		{"https://www.tiktok.com/d/1/ZP9fRX5EnCuJM-sKkqX/", true},
		{"https://www.tiktok.com/t/ZTPR5DJn1/", true},
		{"https://vm.tiktok.com/ZSp6qLWkp/", true},
		{"https://vt.tiktok.com/ZSp6qLWkp/", true},
		{"https://vm.tiktok.com/", false},
		// instagram
		{"https://www.instagram.com/reel/abc/", true},
		{"https://www.instagram.com/someone/reel/abc/", true},
		{"https://www.instagram.com/p/abc/", true},
		{"https://www.instagram.com/share/reel/abc/", true},
		{"https://www.instagram.com/someone/", false},
		{"https://www.instagram.com/explore/tags/cats/", false},
		{"https://www.instagram.com/stories/someone/123/", false},
		// others
		{"https://www.youtube.com/shorts/abc", true},
		{"https://www.youtube.com/@channel", false},
		{"https://clips.twitch.tv/Slug", true},
		{"https://www.twitch.tv/streamer", false},
		{"https://bsky.app/profile/a.bsky.social/post/3k", true},
		{"https://bsky.app/profile/a.bsky.social", false},
		{"https://www.reddit.com/r/videos/comments/abc/title/", true},
		{"https://www.reddit.com/r/videos/", false},
		{"https://v.redd.it/abc123", true},
		{"https://example.com/whatever", true},
		{"ftp://x.com/user/status/1", false},
		{"not a url", false},
	}
	for _, tt := range tests {
		if got := ProbablyVideo(tt.url); got != tt.want {
			t.Errorf("ProbablyVideo(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}
