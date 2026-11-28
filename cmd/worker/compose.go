package main

import (
	"strconv"
	"strings"

	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/store"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// compose is the template and the data of a delivery: the values of the event and the ones of the recipient (the name
// wins). For a digest (events attached to the delivery) the template is `<template>_digest` and the data gets `count`
// and `items`, one line per event, the first event first; the other values are those of the first event.
func compose(r *render.Renderer, cat *catalog.Catalog, w store.Work, items []store.Event, locale string) (string, map[string]string) {
	merge := func(event map[string]string) map[string]string {
		out := make(map[string]string, len(event)+len(w.Delivery.Vars)+2)
		for k, v := range event {
			out[k] = v
		}
		for k, v := range w.Delivery.Vars {
			out[k] = v
		}
		return out
	}
	template := cat.TemplateFor(w.Event.EventType)
	data := merge(w.Event.Data)
	if len(items) == 0 {
		return template, data
	}
	lines := []string{eventLine(r, template, locale, data)}
	for _, it := range items {
		lines = append(lines, eventLine(r, cat.TemplateFor(it.EventType), locale, merge(it.Data)))
	}
	data["count"] = strconv.Itoa(len(lines))
	data["items"] = strings.Join(lines, "\n")
	return template + "_digest", data
}

// eventLine is one event in one line: its inbox body, else its Telegram text, else its e-mail subject.
func eventLine(r *render.Renderer, template, locale string, data map[string]string) string {
	for _, try := range []struct {
		ch   courierv1.Channel
		text func(m string, subject string) string
	}{
		{courierv1.Channel_CHANNEL_IN_APP, func(body, _ string) string { return body }},
		{courierv1.Channel_CHANNEL_TELEGRAM, func(body, _ string) string { return body }},
		{courierv1.Channel_CHANNEL_EMAIL, func(_, subject string) string { return subject }},
	} {
		if !r.Has(locale, template, try.ch) {
			continue
		}
		m, err := r.Render(locale, template, try.ch, data)
		if err != nil {
			continue
		}
		if line := strings.Join(strings.Fields(try.text(m.Text, m.Subject)), " "); line != "" {
			return line
		}
	}
	return template
}
