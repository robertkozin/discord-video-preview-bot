package canary

import (
	"fmt"
	"testing"
	"time"
)

// A tested link that trim dropped must not come back as pending when the
// source lists it again.
func TestTrimmedTestedLinkIsNotReadded(t *testing.T) {
	r := &Runner{items: map[string]*Item{}, lastTested: map[string]time.Time{}, tested: map[string]time.Time{}}

	old := Link{URL: "https://x.com/a/status/1"}
	r.add(old)
	if _, ok := r.pick(); !ok {
		t.Fatal("nothing picked")
	}
	r.items[old.URL].Status = "no_video"

	// push it out with newer links
	for i := 0; i < maxItems+1; i++ {
		r.add(Link{URL: fmt.Sprintf("https://x.com/b/status/%d", i+100)})
	}
	if _, ok := r.items[old.URL]; ok {
		t.Fatal("tested link was not trimmed")
	}

	if r.add(old) {
		t.Error("trimmed tested link was added again")
	}
}
