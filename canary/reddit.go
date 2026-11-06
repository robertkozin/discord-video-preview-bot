package canary

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const redditUserAgent = "linux:discord-video-preview-canary:v1 (homelab link tester)"

var defaultRedditDomains = []string{"v.redd.it", "x.com", "tiktok.com", "instagram.com"}

// Reddit finds links in Reddit's per-domain "hot" listings, e.g.
// /domain/tiktok.com/hot, which only hold posts linking to that domain, so
// the links are already on the right site (v.redd.it is Reddit-hosted video).
//
//	reddit://?domains=v.redd.it&domains=x.com&sort=hot&limit=50
//
// It needs REDDIT_CLIENT_ID, REDDIT_CLIENT_SECRET and REDDIT_REFRESH_TOKEN:
// Reddit blocks anonymous .json, and the OAuth API also returns post metadata,
// which is used to drop NSFW and removed posts (NSFW often needs a login to
// download, which would read as an extractor failure) and to keep only video
// posts on v.redd.it.
type Reddit struct {
	Domains []string
	Sort    string // hot, new, top, rising
	Limit   int

	client                               *http.Client
	clientID, clientSecret, refreshToken string

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

func NewReddit(config *url.URL) (*Reddit, error) {
	q := config.Query()
	r := &Reddit{
		Domains:      q["domains"],
		Sort:         q.Get("sort"),
		Limit:        50,
		client:       &http.Client{Timeout: 20 * time.Second},
		clientID:     os.Getenv("REDDIT_CLIENT_ID"),
		clientSecret: os.Getenv("REDDIT_CLIENT_SECRET"),
		refreshToken: os.Getenv("REDDIT_REFRESH_TOKEN"),
	}
	if len(r.Domains) == 0 {
		r.Domains = defaultRedditDomains
	}
	if r.Sort == "" {
		r.Sort = "hot"
	}
	if l := q.Get("limit"); l != "" {
		if _, err := fmt.Sscanf(l, "%d", &r.Limit); err != nil || r.Limit < 1 || r.Limit > 100 {
			return nil, fmt.Errorf("reddit limit must be 1-100, got %q", l)
		}
	}
	switch r.Sort {
	case "hot", "new", "top", "rising":
	default:
		return nil, fmt.Errorf("unknown reddit sort %q", r.Sort)
	}
	if r.clientID == "" || r.clientSecret == "" || r.refreshToken == "" {
		return nil, fmt.Errorf("reddit source needs REDDIT_CLIENT_ID, REDDIT_CLIENT_SECRET and REDDIT_REFRESH_TOKEN")
	}
	return r, nil
}

func (r *Reddit) String() string { return "reddit" }

func (r *Reddit) token(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accessToken != "" && time.Until(r.tokenExpiry) > 5*time.Minute {
		return r.accessToken, nil
	}

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r.refreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.reddit.com/api/v1/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(r.clientID, r.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", redditUserAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("refreshing reddit token: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("refreshing reddit token: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return "", fmt.Errorf("refreshing reddit token: %s %s", resp.Status, tok.Error)
	}
	r.accessToken = tok.AccessToken
	r.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return r.accessToken, nil
}

type redditListing struct {
	Data struct {
		Children []struct {
			Data struct {
				Title        string `json:"title"`
				URL          string `json:"url"`
				Permalink    string `json:"permalink"`
				Subreddit    string `json:"subreddit_name_prefixed"`
				Score        int    `json:"score"`
				IsVideo      bool   `json:"is_video"`
				Over18       bool   `json:"over_18"`
				RemovedByCat string `json:"removed_by_category"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// Fetch returns links from every domain's listing. A domain that fails is
// reported in the error, and the others' links are still returned.
func (r *Reddit) Fetch(ctx context.Context) ([]Link, error) {
	token, err := r.token(ctx)
	if err != nil {
		return nil, err
	}

	var links []Link
	var errs []string
	for _, domain := range r.Domains {
		listing, err := r.listing(ctx, token, domain)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", domain, err))
			continue
		}
		for _, c := range listing.Data.Children {
			p := c.Data
			if p.Over18 || p.RemovedByCat != "" {
				continue
			}
			link := Link{
				Source:    r.String(),
				Title:     p.Title,
				Context:   p.Subreddit,
				Permalink: "https://www.reddit.com" + p.Permalink,
				Score:     p.Score,
			}
			if domain == "v.redd.it" {
				// the discussion link is what people paste; only hosted videos qualify
				if !p.IsVideo || p.Permalink == "" {
					continue
				}
				link.URL = link.Permalink
			} else {
				u, err := url.Parse(p.URL)
				if err != nil || !strings.HasSuffix(strings.TrimPrefix(u.Hostname(), "www."), domain) {
					continue
				}
				link.URL = p.URL
			}
			links = append(links, link)
		}
	}
	if len(errs) > 0 {
		return links, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return links, nil
}

func (r *Reddit) listing(ctx context.Context, token, domain string) (listing redditListing, err error) {
	u := fmt.Sprintf("https://oauth.reddit.com/domain/%s/%s?limit=%d&raw_json=1", url.PathEscape(domain), r.Sort, r.Limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return listing, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", redditUserAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		return listing, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return listing, err
	}
	if resp.StatusCode != http.StatusOK {
		return listing, fmt.Errorf("%s", resp.Status)
	}
	err = json.Unmarshal(body, &listing)
	return listing, err
}
