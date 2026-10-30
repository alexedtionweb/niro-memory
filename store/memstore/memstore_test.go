package memstore_test

import (
	"testing"

	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
	"github.com/alexedtionweb/niro-memory/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return memstore.New(nil) })
}
