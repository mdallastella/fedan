package opml

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	feeds, err := Parse(strings.NewReader(`<?xml version="1.0"?>
<opml version="1.0"><head><title>Subs</title></head><body>
  <outline text="Tech">
    <outline text="Go">
      <outline text="Go Blog" type="rss" xmlUrl="https://go.dev/blog/feed.atom" htmlUrl="https://go.dev/blog"/>
    </outline>
  </outline>
  <outline text="Status" title="Status page" xmlUrl="https://status.example.com/rss" category="/ops/alerts,security"/>
</body></opml>`))
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 2 {
		t.Fatalf("feeds = %+v", feeds)
	}
	if f := feeds[0]; f.Title != "Go Blog" || f.SiteURL != "https://go.dev/blog" || !slices.Equal(f.Tags, []string{"Tech", "Go"}) {
		t.Errorf("nested feed = %+v", f)
	}
	if f := feeds[1]; f.Title != "Status page" || !slices.Equal(f.Tags, []string{"ops", "alerts", "security"}) {
		t.Errorf("categorized feed = %+v", f)
	}
	if _, err := Parse(strings.NewReader("not xml")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestRoundTrip(t *testing.T) {
	in := []Feed{{URL: "https://a.example/feed", Title: "A & B", SiteURL: "https://a.example", Tags: []string{"main", "news"}}}
	var b bytes.Buffer
	if err := Write(&b, "fēdan", in); err != nil {
		t.Fatal(err)
	}
	out, err := Parse(&b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].URL != in[0].URL || out[0].Title != in[0].Title || !slices.Equal(out[0].Tags, in[0].Tags) {
		t.Errorf("round trip = %+v", out)
	}
}
