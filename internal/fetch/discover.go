package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
)

var feedTypes = []string{"application/rss+xml", "application/atom+xml", "application/feed+json", "application/json"}

// Discover resolves a URL to a feed: the URL itself if it serves one, else
// the first feed a web page advertises with <link rel="alternate">. It returns
// the feed URL and title.
func (f *Fetcher) Discover(ctx context.Context, rawURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", errors.New("enter an http or https URL")
	}
	final, body, err := f.get(ctx, u.String())
	if err != nil {
		return "", "", err
	}
	if parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body)); err == nil {
		return final.String(), strings.TrimSpace(parsed.Title), nil
	}
	link := alternateLink(body)
	if link == "" {
		return "", "", errors.New("no feed found at this URL")
	}
	ref, err := url.Parse(link)
	if err != nil {
		return "", "", fmt.Errorf("bad feed link %q", link)
	}
	feedURL := final.ResolveReference(ref)
	_, body, err = f.get(ctx, feedURL.String())
	if err != nil {
		return "", "", err
	}
	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("%s does not serve a valid feed: %w", feedURL, err)
	}
	return feedURL.String(), strings.TrimSpace(parsed.Title), nil
}

// get fetches a URL and returns the final URL after redirects and the body.
func (f *Fetcher) get(ctx context.Context, rawURL string) (*url.URL, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", f.opts.UserAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s: %s", rawURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxBody {
		return nil, nil, fmt.Errorf("response larger than %d bytes", maxBody)
	}
	return resp.Request.URL, body, nil
}

// alternateLink returns the href of the first feed <link> in an HTML page.
func alternateLink(page []byte) string {
	z := html.NewTokenizer(bytes.NewReader(page))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) == "body" {
				return ""
			}
			if string(name) != "link" || !hasAttr {
				continue
			}
			var rel, typ, href string
			for {
				k, v, more := z.TagAttr()
				switch string(k) {
				case "rel":
					rel = strings.ToLower(string(v))
				case "type":
					typ = strings.ToLower(strings.TrimSpace(string(v)))
				case "href":
					href = string(v)
				}
				if !more {
					break
				}
			}
			if href != "" && strings.Contains(" "+rel+" ", " alternate ") && slices.Contains(feedTypes, typ) {
				return href
			}
		}
	}
}
