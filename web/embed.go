// Package web exposes the embedded dashboard build (web/dist).
package web

import "embed"

// Dist is the production dashboard build, created by `bun run build`.
// A placeholder index.html keeps local Go builds working before then.
//
//go:embed dist
var Dist embed.FS
