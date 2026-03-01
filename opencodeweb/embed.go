package opencodeweb

import "embed"

//go:embed all:dist
var StaticFS embed.FS
