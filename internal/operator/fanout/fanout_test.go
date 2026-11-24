package fanout

import (
	"context"
	"testing"
	"time"
)

func TestEachOwnDeadline(t *testing.T) {
	start := time.Now()
	// La première cible ne répond jamais : les autres répondent quand même.
	got := Each(context.Background(), []int{0, 1, 2, 3}, 100*time.Millisecond, func(ctx context.Context, i int) string {
		if i == 0 {
			<-ctx.Done()
			return "injoignable"
		}
		return "ok"
	})
	if got[0] != "injoignable" || got[1] != "ok" || got[3] != "ok" {
		t.Fatal(got)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("délais additionnés : %v", d)
	}
}

func TestEachLimit(t *testing.T) {
	var cur, max int
	ch := make(chan int, 128)
	items := make([]int, 40)
	Each(context.Background(), items, time.Second, func(ctx context.Context, _ int) int {
		ch <- 1
		time.Sleep(5 * time.Millisecond)
		ch <- -1
		return 0
	})
	close(ch)
	for d := range ch {
		cur += d
		if cur > max {
			max = cur
		}
	}
	if max > Limit {
		t.Fatalf("%d appels simultanés", max)
	}
}
