// Package canary keeps testing the extractor chain against real links.
//
// Every RefreshInterval it fetches fresh links from its Sources (Reddit's hot
// listings per domain) and keeps them in memory. Every Interval it takes one
// not-yet-tested link, rotating between sites so rare ones still get tested,
// and runs it through a Reuploader that discards its output. Each check is its
// own trace ("canary_check", canary=true), so failures are found with the
// existing tracing, e.g. {name="canary_check" && status=error}. The links and
// their results are also listed on the debug page.
package canary

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/preview"
	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var tracer = otel.Tracer("canary")

const (
	maxItems     = 300 // links kept in memory
	checkTimeout = 4 * time.Minute
	// testedTTL is how long a tested link is remembered after trim drops it,
	// so a source that still lists it (a hot listing changes slowly) doesn't
	// bring it back as a new link to test again.
	testedTTL = 48 * time.Hour
)

// Link is a candidate link and what its source knows about it.
type Link struct {
	URL       string
	Source    string // e.g. "reddit"
	Title     string
	Context   string // where it was posted, e.g. "r/videos"
	Permalink string // the discussion, when the source has one
	Score     int
}

// Source finds links to test.
type Source interface {
	fmt.Stringer
	Fetch(ctx context.Context) ([]Link, error)
}

// Item is a link and the state of its test.
type Item struct {
	Link
	Site    string
	FoundAt time.Time

	// set once tested
	Status    string // pending / ok / no_video / failed
	TestedAt  time.Time
	Extractor string            // the extractor that produced the media, when it succeeded
	Metadata  *preview.Metadata // what the extractors knew about the post, when tested and found
	Duration  time.Duration
	TraceID   string
	Error     string
}

type Runner struct {
	Sources         []Source
	Reuploader      *preview.Reuploader // output must go to a discarding destination
	Interval        time.Duration       // between checks
	RefreshInterval time.Duration       // between fetching new links

	mu         sync.Mutex
	items      map[string]*Item     // by URL
	lastTested map[string]time.Time // by site
	tested     map[string]time.Time // by URL: every link tested within testedTTL
}

// Items returns a snapshot of every link held, tested or not.
func (r *Runner) Items() []Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]Item, 0, len(r.items))
	for _, it := range r.items {
		items = append(items, *it)
	}
	return items
}

func (r *Runner) Run(ctx context.Context) {
	r.mu.Lock()
	r.items = map[string]*Item{}
	r.lastTested = map[string]time.Time{}
	r.tested = map[string]time.Time{}
	r.mu.Unlock()

	fmt.Printf("canary: sources=%v check every %s, refresh every %s\n", r.Sources, r.Interval, r.RefreshInterval)

	go func() {
		for {
			r.refresh(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.RefreshInterval):
			}
		}
	}()

	wait := 30 * time.Second // first check soon after startup, once links are in
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if it, ok := r.pick(); ok {
			r.check(ctx, it)
		} else {
			fmt.Println("canary: nothing to test yet")
		}
		wait = r.Interval
	}
}

// refresh fetches new links from every source and adds the ones that are
// probably videos and that an extractor supports.
func (r *Runner) refresh(ctx context.Context) {
	r.forgetTested()
	var added, skipped int
	for _, source := range r.Sources {
		links, err := source.Fetch(ctx)
		if err != nil {
			fmt.Printf("canary: fetching %s: %v\n", source, err)
		}
		for _, l := range links {
			if !preview.ProbablyVideo(l.URL) || !r.Reuploader.IsSupported(l.URL) {
				skipped++
				continue
			}
			if r.add(l) {
				added++
			}
		}
	}
	fmt.Printf("canary: refreshed, %d new links (%d skipped as not video or unsupported)\n", added, skipped)
}

func (r *Runner) add(l Link) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[l.URL]; ok {
		return false
	}
	if _, ok := r.tested[l.URL]; ok {
		return false
	}
	r.items[l.URL] = &Item{Link: l, Site: preview.SiteOf(l.URL), FoundAt: time.Now(), Status: "pending"}
	r.trim()
	return true
}

// forgetTested drops tested links older than testedTTL from the tested set.
func (r *Runner) forgetTested() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for u, at := range r.tested {
		if time.Since(at) > testedTTL {
			delete(r.tested, u)
		}
	}
}

// trim drops the oldest tested links, then the oldest pending ones, once over maxItems.
func (r *Runner) trim() {
	if len(r.items) <= maxItems {
		return
	}
	all := make([]*Item, 0, len(r.items))
	for _, it := range r.items {
		all = append(all, it)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if (a.Status == "pending") != (b.Status == "pending") {
			return a.Status != "pending" // tested links go first
		}
		return a.FoundAt.Before(b.FoundAt)
	})
	for _, it := range all[:len(all)-maxItems] {
		delete(r.items, it.URL)
	}
}

// pick takes the best pending link (highest score) from the site that was
// tested longest ago, never-tested sites first, so rare sites aren't starved
// by busy ones. It returns a copy and marks the link as being tested.
func (r *Runner) pick() (Item, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	best := map[string]*Item{} // site -> best pending link
	for _, it := range r.items {
		if it.Status != "pending" {
			continue
		}
		if b := best[it.Site]; b == nil || it.Score > b.Score || (it.Score == b.Score && it.FoundAt.After(b.FoundAt)) {
			best[it.Site] = it
		}
	}
	var chosen *Item
	for site, it := range best {
		if chosen == nil || r.lastTested[site].Before(r.lastTested[chosen.Site]) {
			chosen = it
		}
	}
	if chosen == nil {
		return Item{}, false
	}
	chosen.Status = "testing"
	r.lastTested[chosen.Site] = time.Now()
	r.tested[chosen.URL] = time.Now()
	return *chosen, true
}

// check runs one link through the extractors as its own root trace.
func (r *Runner) check(ctx context.Context, it Item) {
	var err error
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	ctx, span := tracer.Start(ctx, "canary_check")
	defer tr.End(span, &err)
	span.SetAttributes(
		attribute.Bool("canary", true),
		attribute.String("canary.source", it.Source),
		attribute.String("canary.url", it.URL),
		attribute.String("canary.context", it.Context),
		attribute.String("site", it.Site),
	)

	// Reupload also tags this span with preview.site / preview.extractor / preview.metadata
	start := time.Now()
	res, err := r.Reuploader.Reupload(ctx, it.URL)
	took := time.Since(start)

	status := "ok"
	switch {
	case errors.Is(err, preview.ErrNoVideo):
		// the post simply has nothing to download, which isn't an extractor failure
		status, err = "no_video", nil
	case err != nil:
		status = "failed"
	}
	span.SetAttributes(
		attribute.String("canary.outcome", status),
		attribute.Int("canary.files", len(res.Links)),
	)

	traceID := span.SpanContext().TraceID().String()
	r.mu.Lock()
	if stored := r.items[it.URL]; stored != nil {
		stored.Status = status
		stored.TestedAt = start
		stored.Extractor = res.Extractor
		stored.Metadata = res.Metadata
		stored.Duration = took
		stored.TraceID = traceID
		if err != nil {
			stored.Error = err.Error()
		}
	}
	r.mu.Unlock()

	if status == "failed" {
		fmt.Printf("canary: FAIL trace=%s site=%s url=%s after=%s err=%v\n", traceID, it.Site, it.URL, took.Round(time.Millisecond), err)
	} else {
		fmt.Printf("canary: %s trace=%s site=%s extractor=%s metadata=%t url=%s after=%s\n", status, traceID, it.Site, res.Extractor, !res.Metadata.Empty(), it.URL, took.Round(time.Millisecond))
	}
}
