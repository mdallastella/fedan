// Package web embeds the UI templates and static assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var static embed.FS

// Static returns the static UI files.
func Static() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
