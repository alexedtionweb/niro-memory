package enginetest_test

import (
	"testing"

	"github.com/alexedtionweb/niro-memory/enginetest"
	"github.com/alexedtionweb/niro-memory/store"
	"github.com/alexedtionweb/niro-memory/store/memstore"
)

func TestMemstore(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) store.Store { return memstore.New(nil) })
}
