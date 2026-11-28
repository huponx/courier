package catalog

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestDefaultCatalogMatchesTemplates(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	keyRe := regexp.MustCompile(`\{\{[^}]*?\.([a-z_]+)`)
	known := map[string]bool{}
	for _, k := range common {
		known[k] = true
	}
	for _, et := range c.Types() {
		for _, k := range append(append([]string{}, et.Required...), et.Optional...) {
			known[k] = true
		}
		for _, locale := range []string{"en", "vi"} {
			dir := filepath.Join("..", "notify", "templates", locale, et.Template)
			used := map[string]bool{}
			err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				b, _ := os.ReadFile(p)
				for _, m := range keyRe.FindAllStringSubmatch(string(b), -1) {
					used[m[1]] = true
				}
				return nil
			})
			if err != nil {
				t.Fatalf("%s/%s: template directory: %v", locale, et.Template, err)
			}
			// every key a template reads must be declared, so a caller can never be told "unknown" for a key the template needs
			var missing []string
			for k := range used {
				if !contains(et.Required, k) && !contains(et.Optional, k) && !contains(common, k) {
					missing = append(missing, k)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s (%s) reads %s but the catalog does not declare it", et.Type, locale, strings.Join(missing, ","))
			}
		}
	}
}

func TestValidate(t *testing.T) {
	c, _ := Default()
	et, _ := c.Get("security.password_reset")
	if err := et.Validate(map[string]string{"link": "x", "expires_in": "30", "brand": "B"}); err != nil {
		t.Fatal(err)
	}
	err := et.Validate(map[string]string{"link": " ", "lnik": "typo"})
	if err == nil || !strings.Contains(err.Error(), "missing link") || !strings.Contains(err.Error(), "missing expires_in") || !strings.Contains(err.Error(), "unknown lnik") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	for name, y := range map[string]string{
		"dup":     "event_types:\n- {type: a, category: c, template: t, channels: [email]}\n- {type: a, category: c, template: t, channels: [email]}",
		"channel": "event_types:\n- {type: a, category: c, template: t, channels: [sms]}",
		"empty":   "event_types:\n- {type: a, category: c, template: t}",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A type that is grouped needs the template `<template>_digest` in every locale for each channel it sends to (the
// inbox files serve push and webhooks), and that template may read the keys of the type, `count` and `items`.
func TestDigestTemplates(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	keyRe := regexp.MustCompile(`\{\{[^}]*?\.([a-z_]+)`)
	grouped := 0
	for _, et := range c.Types() {
		if et.Digest == nil {
			continue
		}
		grouped++
		files := map[string][]string{"email": {"email.subject.tmpl", "email.txt.tmpl"}, "telegram": {"telegram.txt.tmpl"}, "push": {"in_app.title.tmpl", "in_app.body.tmpl"}}
		for _, locale := range []string{"en", "vi"} {
			dir := filepath.Join("..", "notify", "templates", locale, et.Template+"_digest")
			for _, ch := range et.Channels {
				for _, f := range files[ch] {
					if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
						t.Errorf("%s: %s/%s is missing for the channel %s", et.Type, locale, f, ch)
					}
				}
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				for _, m := range keyRe.FindAllStringSubmatch(string(b), -1) {
					k := m[1]
					if k != "count" && k != "items" && !contains(et.Required, k) && !contains(et.Optional, k) && !contains(common, k) {
						t.Errorf("%s: %s/%s reads %s", et.Type, locale, e.Name(), k)
					}
				}
			}
		}
	}
	if grouped == 0 {
		t.Fatal("no digest type in the catalog")
	}
}

func TestParseDigest(t *testing.T) {
	base := "event_types:\n- {type: a, category: c, template: t, channels: [email], required: [k], %s}"
	for name, d := range map[string]string{
		"ok":          "digest: {window: 10m, group_by: [k]}",
		"too short":   "digest: {window: 10s}",
		"too long":    "digest: {window: 48h}",
		"no window":   "digest: {group_by: [k]}",
		"unknown key": "digest: {window: 10m, group_by: [nope]}",
		"mandatory":   "mandatory: true, digest: {window: 10m}",
	} {
		_, err := Parse([]byte(strings.Replace(base, "%s", d, 1)))
		if (err == nil) != (name == "ok") {
			t.Errorf("%s: %v", name, err)
		}
	}
	et := &Digest{GroupBy: []string{"step", "tenant"}}
	if k := et.Key("bvchain.failed", map[string]string{"step": "login", "tenant": "t1", "x": "y"}); k != "bvchain.failed|login|t1" {
		t.Errorf("key %q", k)
	}
}
