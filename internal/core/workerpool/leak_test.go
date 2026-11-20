package workerpool

import (
	"testing"

	"go.uber.org/goleak"
)

// Aucune goroutine ne doit survivre aux tests du paquet.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
