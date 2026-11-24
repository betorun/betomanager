package repository

import (
	"errors"
	"testing"
	"time"
)

var errStill = errors.New("pas encore")

func waitFor(t *testing.T, f func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("délai dépassé : %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
