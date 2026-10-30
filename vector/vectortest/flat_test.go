package vectortest_test

import (
	"testing"

	"github.com/alexedtionweb/niro-memory/vector"
	"github.com/alexedtionweb/niro-memory/vector/vectortest"
)

func TestFlat(t *testing.T) {
	vectortest.Run(t, func(t *testing.T) vector.Index { return vector.NewFlat() })
}
