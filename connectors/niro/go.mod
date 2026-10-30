module github.com/alexedtionweb/niro-memory/connectors/niro

go 1.23.0

require (
	github.com/alexedtionweb/niro-memory v0.0.0
	github.com/alexedtionweb/niro-stream v0.5.0-alpha
)

// Development: resolve the core module from this repository.
replace github.com/alexedtionweb/niro-memory => ../..
