package render

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"strings"
	texttemplate "text/template"

	"github.com/huponx/courier/internal/notify"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

type Renderer struct {
	// fs is what is read: the edited templates over base (the built-in ones). overlay is nil for a renderer
	// built with NewFS on a file system of its own.
	fs            fs.FS
	base          fs.FS
	overlay       *Overlay
	defaultLocale string
	defaults      map[string]string
}

// SetDefaults sets template data every message gets unless the caller sent the key itself (the brand and the logo of the
// emails, for example). An empty value is not a default.
func (r *Renderer) SetDefaults(d map[string]string) {
	r.defaults = map[string]string{}
	for k, v := range d {
		if v != "" {
			r.defaults[k] = v
		}
	}
}

func (r *Renderer) withDefaults(data map[string]string) map[string]string {
	out := make(map[string]string, len(data)+len(r.defaults))
	for k, v := range r.defaults {
		out[k] = v
	}
	for k, v := range data {
		if v != "" || r.defaults[k] == "" {
			out[k] = v
		}
	}
	return out
}

func New(defaultLocale string) *Renderer {
	sub, err := fs.Sub(notify.TemplateFS, "templates")
	if err != nil {
		sub = notify.TemplateFS
	}
	return NewFS(sub, defaultLocale)
}

func NewFS(fsys fs.FS, defaultLocale string) *Renderer {
	if defaultLocale == "" {
		defaultLocale = "en"
	}
	ov := NewOverlay(fsys)
	return &Renderer{fs: ov, base: fsys, overlay: ov, defaultLocale: strings.ToLower(defaultLocale)}
}

func NormalizeLocale(locale string) string {
	return strings.ToLower(strings.TrimSpace(locale))
}

func (r *Renderer) Has(locale, templateID string, ch courierv1.Channel) bool {
	_, err := r.resolveDir(locale, templateID, ch)
	return err == nil
}

func (r *Renderer) Render(locale, templateID string, ch courierv1.Channel, data map[string]string) (notify.Message, error) {
	dir, err := r.resolveDir(locale, templateID, ch)
	if err != nil {
		return notify.Message{}, err
	}
	slug, _ := notify.TemplateSlug(ch)
	data = r.withDefaults(data)

	var msg notify.Message
	switch ch {
	case courierv1.Channel_CHANNEL_EMAIL:
		msg.Subject, err = execText(r.fs, path.Join(dir, slug+".subject.tmpl"), data)
		if err != nil {
			return notify.Message{}, err
		}
		if exists(r.fs, path.Join(dir, slug+".txt.tmpl")) {
			msg.Text, err = execText(r.fs, path.Join(dir, slug+".txt.tmpl"), data)
			if err != nil {
				return notify.Message{}, err
			}
		}
		if exists(r.fs, path.Join(dir, slug+".html.tmpl")) {
			msg.HTML, err = execHTML(r.fs, path.Join(dir, slug+".html.tmpl"), path.Join(path.Dir(dir), layoutFile), data)
			if err != nil {
				return notify.Message{}, err
			}
		}
	case courierv1.Channel_CHANNEL_TELEGRAM:
		msg.Text, err = execText(r.fs, path.Join(dir, slug+".txt.tmpl"), data)
		if err != nil {
			return notify.Message{}, err
		}
	case courierv1.Channel_CHANNEL_IN_APP, courierv1.Channel_CHANNEL_PUSH:
		// the title is the Subject, the body is the Text
		msg.Subject, err = execText(r.fs, path.Join(dir, slug+".title.tmpl"), data)
		if err != nil {
			return notify.Message{}, err
		}
		msg.Text, err = execText(r.fs, path.Join(dir, slug+".body.tmpl"), data)
		if err != nil {
			return notify.Message{}, err
		}
	default:
		return notify.Message{}, fmt.Errorf("unsupported channel %s", ch.String())
	}
	return msg, nil
}

func (r *Renderer) resolveDir(locale, templateID string, ch courierv1.Channel) (string, error) {
	slug, ok := notify.TemplateSlug(ch)
	if !ok {
		return "", fmt.Errorf("unknown channel")
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return "", fmt.Errorf("template_id is required")
	}
	for _, loc := range []string{NormalizeLocale(locale), r.defaultLocale} {
		if loc == "" {
			continue
		}
		dir := path.Join(loc, templateID)
		if r.hasParts(dir, slug, ch) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("template %q not found for locale %q channel %s", templateID, locale, ch.String())
}

func (r *Renderer) hasParts(dir, slug string, ch courierv1.Channel) bool {
	switch ch {
	case courierv1.Channel_CHANNEL_EMAIL:
		if !exists(r.fs, path.Join(dir, slug+".subject.tmpl")) {
			return false
		}
		return exists(r.fs, path.Join(dir, slug+".txt.tmpl")) || exists(r.fs, path.Join(dir, slug+".html.tmpl"))
	case courierv1.Channel_CHANNEL_TELEGRAM:
		return exists(r.fs, path.Join(dir, slug+".txt.tmpl"))
	case courierv1.Channel_CHANNEL_IN_APP, courierv1.Channel_CHANNEL_PUSH:
		return exists(r.fs, path.Join(dir, slug+".title.tmpl")) && exists(r.fs, path.Join(dir, slug+".body.tmpl"))
	default:
		return false
	}
}

func exists(fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// funcs are what the templates can call: lines splits a digest's `items` (one event per line) so a template can lay
// them out as rows; it is also what lets an HTML mail escape every line on its own.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

var (
	htmlFuncs = template.FuncMap{"lines": lines}
	textFuncs = texttemplate.FuncMap{"lines": lines}
)

func execText(fsys fs.FS, name string, data map[string]string) (string, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	tpl, err := texttemplate.New(path.Base(name)).Funcs(textFuncs).Option("missingkey=zero").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("exec %s: %w", name, err)
	}
	return buf.String(), nil
}

// layoutFile is the page every HTML mail of a locale is set into (<locale>/layout.html.tmpl): the logo, the card and the
// footer. The page of a template is its "content" block; a locale without a layout sends the page as it is.
const layoutFile = "layout.html.tmpl"

func execHTML(fsys fs.FS, name, layout string, data map[string]string) (string, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	run := path.Base(name)
	var tpl *template.Template
	if exists(fsys, layout) {
		lraw, err := fs.ReadFile(fsys, layout)
		if err != nil {
			return "", err
		}
		if tpl, err = template.New("layout").Funcs(htmlFuncs).Option("missingkey=zero").Parse(string(lraw)); err != nil {
			return "", fmt.Errorf("parse %s: %w", layout, err)
		}
		if _, err = tpl.New("content").Parse(string(raw)); err != nil {
			return "", fmt.Errorf("parse %s: %w", name, err)
		}
		run = "layout"
	} else if tpl, err = template.New(run).Funcs(htmlFuncs).Option("missingkey=zero").Parse(string(raw)); err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, run, data); err != nil {
		return "", fmt.Errorf("exec %s: %w", name, err)
	}
	return buf.String(), nil
}
