package preview

import (
	"net/url"
	"testing"
)

func TestTikTokShortLinkClassification(t *testing.T) {
	short := map[string]bool{
		"https://www.tiktok.com/t/ZP9faBukAWb6R-HriUm/":   true,
		"https://pro.tiktok.com/t/ZP9fa6RGH7C4K-F40qZ/":   true,
		"https://vm.tiktok.com/ZMabc123/":                 true,
		"https://vt.tiktok.com/ZSabc123/":                 true,
		"https://www.tiktok.com/@user/video/123":          false,
		"https://www.tiktok.com/t/":                       false,
		"https://www.instagram.com/t/ZP9faBukAWb6R-HriUm": false,
	}
	for raw, want := range short {
		u, _ := url.Parse(raw)
		if got := isTikTokShortLink(u); got != want {
			t.Errorf("isTikTokShortLink(%s) = %v, want %v", raw, got, want)
		}
	}

	nonVideo := map[string]bool{
		"https://www.tiktok.com/view/product/1732837595416859453?_d=x":   true,
		"https://www.tiktok.com/ug/incentive/share/push_coin?aid=473824": true,
		"https://www.tiktok.com/@user/video/123":                         false,
		"https://www.tiktok.com/@user/photo/123":                         false,
		"https://www.tiktok.com/":                                        false,
		"https://example.com/view/product/1":                             false,
	}
	for raw, want := range nonVideo {
		u, _ := url.Parse(raw)
		if got := tikTokNonVideo(u); got != want {
			t.Errorf("tikTokNonVideo(%s) = %v, want %v", raw, got, want)
		}
	}
}
