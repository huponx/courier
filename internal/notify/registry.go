package notify

import (
	"context"
	"fmt"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

type Registry struct {
	channels map[courierv1.Channel]Channel
}

func NewRegistry(cs ...Channel) *Registry {
	r := &Registry{channels: make(map[courierv1.Channel]Channel, len(cs))}
	for _, c := range cs {
		r.Register(c)
	}
	return r
}

func (r *Registry) Register(c Channel) {
	if c == nil {
		return
	}
	r.channels[c.ID()] = c
}

func (r *Registry) Lookup(ch courierv1.Channel) (Channel, bool) {
	c, ok := r.channels[ch]
	return c, ok
}

func (r *Registry) Has(ch courierv1.Channel) bool {
	_, ok := r.channels[ch]
	return ok
}

func (r *Registry) Send(ctx context.Context, ch courierv1.Channel, dest Destination, msg Message) error {
	c, ok := r.Lookup(ch)
	if !ok {
		return fmt.Errorf("channel not registered: %s", ch.String())
	}
	return c.Send(ctx, dest, msg)
}
