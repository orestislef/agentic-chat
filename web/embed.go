// Package web embeds the browser UI into the binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var files embed.FS

// FS serves the UI files from the root of the site.
var FS = mustSub(files, "static")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
