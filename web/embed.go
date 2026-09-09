// Package web carries the browser assets built into the daemon binary.
//
// Embedding means the machine that serves a review needs no Gren toolchain and
// no Node: build once, copy one binary. The dev build tag swaps in a
// filesystem-backed variant so front-end iteration does not require relinking.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

//go:embed dist
var distFS embed.FS

// Static returns the hand-written assets: index.html, ports.js, style.css.
func Static() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // impossible: the directory is embedded above
	}
	return sub
}

// Dist returns the compiled Gren output.
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
