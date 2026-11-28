// Package catalog is the list of event types courier accepts in Notify: the contract between the services
// that raise events and the templates that show them. It is read from event_types.yaml, embedded in the binary.
package catalog

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

//go:embed event_types.yaml
var defaultYAML []byte

// Always accepted in `data` besides the keys of the type: `name` (courier fills it from the user),
// `brand` and `logo_url` (the header of the HTML mails).
var common = []string{"name", "brand", "logo_url"}

type EventType struct {
	Type      string   `yaml:"type"`
	Category  string   `yaml:"category"`
	Template  string   `yaml:"template"`
	Channels  []string `yaml:"channels"`
	Mandatory bool     `yaml:"mandatory"`
	// Broadcast: an administrator may send this type to an audience (AdminNotificationService.CreateBroadcast).
	Broadcast bool `yaml:"broadcast"`
	// Digest groups events of this type that arrive close together into one message per recipient and channel; the
	// inbox is not grouped (every item shows in the bell). Never for a mandatory type.
	Digest *Digest `yaml:"digest"`
	// Severity of the inbox item: info (default), warning or critical.
	Severity string   `yaml:"severity"`
	Required []string `yaml:"required"`
	Optional []string `yaml:"optional"`
}

// Digest says how the events of a type are grouped.
type Digest struct {
	// Window is how long the first event of a group waits for the others (the group is not extended by later events).
	Window time.Duration `yaml:"-"`
	// WindowText is Window as written in the YAML ("15m").
	WindowText string `yaml:"window"`
	// GroupBy are keys of `data`: events with the same values are one group. Empty: all events of the type.
	GroupBy []string `yaml:"group_by"`
}

// Key is the group of an event: its type and the values of GroupBy.
func (d *Digest) Key(eventType string, data map[string]string) string {
	k := eventType
	for _, g := range d.GroupBy {
		k += "|" + data[g]
	}
	return k
}

type Catalog struct {
	types map[string]EventType
}

// Default parses the embedded catalog.
func Default() (*Catalog, error) { return Parse(defaultYAML) }

// Parse reads and checks a catalog: every type is named once, has a template and at least one known channel.
func Parse(raw []byte) (*Catalog, error) {
	var doc struct {
		EventTypes []EventType `yaml:"event_types"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	c := &Catalog{types: make(map[string]EventType, len(doc.EventTypes))}
	for _, et := range doc.EventTypes {
		et.Type = strings.TrimSpace(et.Type)
		if et.Type == "" || et.Template == "" || et.Category == "" {
			return nil, fmt.Errorf("catalog: type, category and template are required (%q)", et.Type)
		}
		if _, dup := c.types[et.Type]; dup {
			return nil, fmt.Errorf("catalog: duplicate type %q", et.Type)
		}
		if len(et.Channels) == 0 {
			return nil, fmt.Errorf("catalog: %s has no channels", et.Type)
		}
		for _, ch := range et.Channels {
			if ch != "email" && ch != "telegram" && ch != "in_app" && ch != "push" {
				return nil, fmt.Errorf("catalog: %s: unknown channel %q", et.Type, ch)
			}
		}
		if et.Digest != nil {
			w, err := time.ParseDuration(et.Digest.WindowText)
			if err != nil || w < time.Minute || w > 24*time.Hour {
				return nil, fmt.Errorf("catalog: %s: digest window must be between 1m and 24h", et.Type)
			}
			if et.Mandatory {
				return nil, fmt.Errorf("catalog: %s: a mandatory type is never grouped", et.Type)
			}
			for _, g := range et.Digest.GroupBy {
				if !contains(et.Required, g) && !contains(et.Optional, g) {
					return nil, fmt.Errorf("catalog: %s: digest group_by %q is not a key of the type", et.Type, g)
				}
			}
			et.Digest.Window = w
		}
		switch et.Severity {
		case "":
			et.Severity = "info"
		case "info", "warning", "critical":
		default:
			return nil, fmt.Errorf("catalog: %s: unknown severity %q", et.Type, et.Severity)
		}
		c.types[et.Type] = et
	}
	return c, nil
}

func (c *Catalog) Get(eventType string) (EventType, bool) {
	et, ok := c.types[eventType]
	return et, ok
}

// Types lists the event types ordered by name.
func (c *Catalog) Types() []EventType {
	out := make([]EventType, 0, len(c.types))
	for _, et := range c.types {
		out = append(out, et)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// Validate checks data against the type: required keys present (and not empty), no unknown keys.
// It returns every problem at once, for the message to the developer of the caller.
func (et EventType) Validate(data map[string]string) error {
	allowed := make(map[string]bool, len(et.Required)+len(et.Optional)+len(common))
	for _, k := range et.Required {
		allowed[k] = true
	}
	for _, k := range et.Optional {
		allowed[k] = true
	}
	for _, k := range common {
		allowed[k] = true
	}
	var problems []string
	for _, k := range et.Required {
		if strings.TrimSpace(data[k]) == "" {
			problems = append(problems, "missing "+k)
		}
	}
	var unknown []string
	for k := range data {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		problems = append(problems, "unknown "+k)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s data: %s", et.Type, strings.Join(problems, ", "))
	}
	return nil
}

// TemplateFor returns the template of an event type; a name that is not in the catalog (an event written by
// the older SendNotification, which names the template itself) is its own template.
func (c *Catalog) TemplateFor(eventType string) string {
	if et, ok := c.types[eventType]; ok {
		return et.Template
	}
	return eventType
}

// Category is a group of event types the user turns on and off together.
type Category struct {
	Name string
	// Mandatory: every event of the category is mandatory, so the user cannot turn it off.
	Mandatory bool
	// Channels used by the events of the category (email, telegram, in_app), in a fixed order.
	Channels []string
}

// Categories lists the categories of the catalog ordered by name.
func (c *Catalog) Categories() []Category {
	byName := map[string]*Category{}
	for _, et := range c.types {
		cat := byName[et.Category]
		if cat == nil {
			cat = &Category{Name: et.Category, Mandatory: true}
			byName[et.Category] = cat
		}
		cat.Mandatory = cat.Mandatory && et.Mandatory
		for _, ch := range et.Channels {
			if !contains(cat.Channels, ch) {
				cat.Channels = append(cat.Channels, ch)
			}
		}
	}
	order := map[string]int{"email": 0, "telegram": 1, "in_app": 2, "push": 3}
	out := make([]Category, 0, len(byName))
	for _, cat := range byName {
		sort.Slice(cat.Channels, func(i, j int) bool { return order[cat.Channels[i]] < order[cat.Channels[j]] })
		out = append(out, *cat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// SampleData fills every key of the type with a sample value, for a preview or the check of a template.
func (et EventType) SampleData() map[string]string {
	out := map[string]string{"name": "Alex", "brand": "Brand"}
	for _, k := range append(append([]string{}, et.Required...), et.Optional...) {
		out[k] = "sample " + k
	}
	return out
}
