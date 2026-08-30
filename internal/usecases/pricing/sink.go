package pricing

import (
	"context"

	"github.com/harrison542002/go-route/internal/core/domains"
	"github.com/harrison542002/go-route/internal/ports"
)

type Sink struct {
	pricer *Pricer
	next   ports.DecisionSink
}

var _ ports.DecisionSink = (*Sink)(nil)

func NewSink(p *Pricer, next ports.DecisionSink) *Sink {
	return &Sink{pricer: p, next: next}
}

func (s *Sink) Record(d domains.RoutingDecision) {
	d.Cost = s.pricer.Price(d)
	s.next.Record(d)
}

func (s *Sink) Flush(ctx context.Context) error {
	return s.next.Flush(ctx)
}
