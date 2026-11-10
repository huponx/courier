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
	fs            fs.FS
	defaultLocale string
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
	return &Renderer{fs: fsys, defaultLocale: strings.ToLower(defaultLocale)}
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
	slug, _ := notify.Slug(ch)
	if data == nil {
		data = map[string]string{}
	}

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
			msg.HTML, err = execHTML(r.fs, path.Join(dir, slug+".html.tmpl"), data)
			if err != nil {
				return notify.Message{}, err
			}
		}
	case courierv1.Channel_CHANNEL_TELEGRAM:
		msg.Text, err = execText(r.fs, path.Join(dir, slug+".txt.tmpl"), data)
		if err != nil {
			return notify.Message{}, err
		}
	default:
		return notify.Message{}, fmt.Errorf("unsupported channel %s", ch.String())
	}
	return msg, nil
}

func (r *Renderer) resolveDir(locale, templateID string, ch courierv1.Channel) (string, error) {
	slug, ok := notify.Slug(ch)
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

func execText(fsys fs.FS, name string, data map[string]string) (string, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	tpl, err := texttemplate.New(path.Base(name)).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("exec %s: %w", name, err)
	}
	return buf.String(), nil
}

func execHTML(fsys fs.FS, name string, data map[string]string) (string, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	tpl, err := template.New(path.Base(name)).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("exec %s: %w", name, err)
	}
	return buf.String(), nil
}
