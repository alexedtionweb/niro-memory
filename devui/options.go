// Package devui is the development console: a zero-build web UI (plain
// HTML, CSS and JavaScript modules) that shows every engine feature while
// you build with it — memories and their version timelines, conversations
// with media, the search planner trace and score explanations, embeddings
// coverage, the entity graph, a live rules tester, jobs and metrics.
//
// It is compiled in only with the build tag "devui"; production binaries
// do not contain it (Enabled is false). cmd/niromem additionally refuses to
// serve it in production or on a non-loopback address. Every request is
// authenticated like the public API and must be an admin identity.
package devui

import (
	niromem "github.com/alexedtionweb/niro-memory"
	"github.com/alexedtionweb/niro-memory/api"
	"github.com/alexedtionweb/niro-memory/metrics"
)

// Options configures the console.
type Options struct {
	Engine  *niromem.Engine
	Auth    api.Authenticator
	Metrics *metrics.Registry
}
