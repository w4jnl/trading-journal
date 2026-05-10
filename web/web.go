// Package web embeds the HTML templates and static assets used by the HTTP
// layer so the binary stays self-contained — `go build` and ship one file.
package web

import "embed"

//go:embed all:templates
var TemplatesFS embed.FS

//go:embed all:static
var StaticFS embed.FS
