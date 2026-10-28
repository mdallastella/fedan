// Package opml reads and writes OPML feed lists. Tags map to OPML categories
// and, on import, to the folders a feed is nested in.
package opml

import (
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Feed is one feed of a list.
type Feed struct {
	URL, Title, SiteURL string
	// Tags are raw names; callers turn them into tags.
	Tags []string
}

type document struct {
	XMLName xml.Name  `xml:"opml"`
	Version string    `xml:"version,attr"`
	Title   string    `xml:"head>title"`
	Created string    `xml:"head>dateCreated,omitempty"`
	Body    []outline `xml:"body>outline"`
}

type outline struct {
	Text     string    `xml:"text,attr"`
	Title    string    `xml:"title,attr,omitempty"`
	Type     string    `xml:"type,attr,omitempty"`
	XMLURL   string    `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string    `xml:"htmlUrl,attr,omitempty"`
	Category string    `xml:"category,attr,omitempty"`
	Children []outline `xml:"outline"`
}

// Parse reads the feeds of an OPML document.
func Parse(r io.Reader) ([]Feed, error) {
	var doc document
	d := xml.NewDecoder(io.LimitReader(r, 10<<20))
	d.Strict = false
	if err := d.Decode(&doc); err != nil {
		return nil, fmt.Errorf("not an OPML file: %w", err)
	}
	var feeds []Feed
	var walk func([]outline, []string)
	walk = func(outlines []outline, folders []string) {
		for _, o := range outlines {
			name := o.Title
			if name == "" {
				name = o.Text
			}
			if o.XMLURL == "" {
				walk(o.Children, append(slices.Clone(folders), name))
				continue
			}
			tags := slices.Clone(folders)
			for c := range strings.SplitSeq(o.Category, ",") {
				// Categories may be paths, like "/tech/go".
				for part := range strings.SplitSeq(c, "/") {
					if part = strings.TrimSpace(part); part != "" {
						tags = append(tags, part)
					}
				}
			}
			feeds = append(feeds, Feed{URL: strings.TrimSpace(o.XMLURL), Title: name, SiteURL: o.HTMLURL, Tags: tags})
		}
	}
	walk(doc.Body, nil)
	return feeds, nil
}

// Write writes feeds as a flat OPML document with tags as categories.
func Write(w io.Writer, title string, feeds []Feed) error {
	doc := document{Version: "2.0", Title: title, Created: time.Now().UTC().Format(time.RFC1123Z)}
	for _, f := range feeds {
		doc.Body = append(doc.Body, outline{
			Text: f.Title, Title: f.Title, Type: "rss",
			XMLURL: f.URL, HTMLURL: f.SiteURL, Category: strings.Join(f.Tags, ","),
		})
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
