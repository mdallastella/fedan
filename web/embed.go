// Package web embeds the UI templates and static assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static all:templates
var files embed.FS

// Static returns the static UI files.
func Static() fs.FS {
	return sub("static")
}

// Templates returns the HTML templates.
func Templates() fs.FS {
	return sub("templates")
}

func sub(dir string) fs.FS {
	s, err := fs.Sub(files, dir)
	if err != nil {
		panic(err)
	}
	return s
}
