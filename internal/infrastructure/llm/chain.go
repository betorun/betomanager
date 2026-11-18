package llm

import (
	"context"
	"errors"
	"iter"
	"strings"
)

// Chain : essaie les fournisseurs dans l'ordre, et passe au suivant sur une
// panne qui le permet (Error.Failover). En flux, la bascule n'a lieu que si
// rien n'a encore été transmis à l'appelant : on ne recolle jamais deux
// réponses.
type Chain struct {
	providers []Provider
}

// NewChain : au moins un fournisseur.
func NewChain(providers ...Provider) (*Chain, error) {
	if len(providers) == 0 {
		return nil, errors.New("llm: chaîne sans fournisseur")
	}
	return &Chain{providers: providers}, nil
}

func (c *Chain) Name() string {
	names := make([]string, len(c.providers))
	for i, p := range c.providers {
		names[i] = p.Name()
	}
	return strings.Join(names, ">")
}

func (c *Chain) Generate(ctx context.Context, req Request) (*Response, error) {
	var last error
	for _, p := range c.providers {
		resp, err := p.Generate(ctx, req)
		if err == nil {
			return resp, nil
		}
		last = err
		if !CanFailover(ctx, err) {
			return nil, err
		}
	}
	return nil, last
}

func (c *Chain) Stream(ctx context.Context, req Request) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		var last error
		for _, p := range c.providers {
			sent := false
			var streamErr error
			for ev, err := range p.Stream(ctx, req) {
				if err != nil {
					streamErr = err
					break
				}
				sent = true
				if !yield(ev, nil) {
					return
				}
			}
			if streamErr == nil {
				return // EventDone transmis
			}
			last = streamErr
			if sent || !CanFailover(ctx, streamErr) {
				yield(Event{}, streamErr)
				return
			}
		}
		yield(Event{}, last)
	}
}
