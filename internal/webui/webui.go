// Package webui embeds the single-page user interface.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// FS returns the embedded assets rooted at the static directory.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
