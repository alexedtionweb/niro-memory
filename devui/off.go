//go:build !devui

package devui

import "net/http"

// Enabled reports whether the console is compiled in (build tag devui).
const Enabled = false

// Handler returns 404 for everything: the console is not compiled in.
func Handler(Options) http.Handler { return http.NotFoundHandler() }
