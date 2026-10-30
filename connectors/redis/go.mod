module github.com/alexedtionweb/niro-memory/connectors/redis

go 1.23.0

require github.com/alexedtionweb/niro-memory v0.0.0

// Development: resolve the core module from this repository.
replace github.com/alexedtionweb/niro-memory => ../..
