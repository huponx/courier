package render

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// Overlay is a file system of edited templates over the built-in ones: a file that was edited in the admin app wins,
// every other file is the built-in one. The renderer reads through it, so it needs no idea of where a template comes
// from. The edited files are replaced as a whole by Set (an atomic swap): a render never sees half of a change.
type Overlay struct {
	base  fs.FS
	files atomic.Pointer[map[string][]byte]
}

func NewOverlay(base fs.FS) *Overlay {
	o := &Overlay{base: base}
	empty := map[string][]byte{}
	o.files.Store(&empty)
	return o
}

// Set replaces the edited files: path (locale/template/file) -> text.
func (o *Overlay) Set(files map[string]string) {
	m := make(map[string][]byte, len(files))
	for p, c := range files {
		m[path.Clean(p)] = []byte(c)
	}
	o.files.Store(&m)
}

// Open implements fs.FS.
func (o *Overlay) Open(name string) (fs.File, error) {
	if data, ok := (*o.files.Load())[path.Clean(name)]; ok {
		if len(data) == 0 {
			// an edited template that has no such part: the built-in one must not come back (an e-mail whose html was
			// cleared would send the old html with the new text)
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return &memFile{name: path.Base(name), Reader: bytes.NewReader(data), size: int64(len(data))}, nil
	}
	return o.base.Open(name)
}

type memFile struct {
	name string
	*bytes.Reader
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) { return memInfo{f.name, f.size}, nil }
func (f *memFile) Close() error               { return nil }

type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }

var _ io.Reader = (*memFile)(nil)

// Channels the templates exist for, by the name the database and the admin API use.
const (
	ChannelEmail    = "email"
	ChannelTelegram = "telegram"
	ChannelInApp    = "in_app"
)

// ChannelOf is the channel of its name; false for any other.
func ChannelOf(name string) (courierv1.Channel, bool) {
	switch name {
	case ChannelEmail:
		return courierv1.Channel_CHANNEL_EMAIL, true
	case ChannelTelegram:
		return courierv1.Channel_CHANNEL_TELEGRAM, true
	case ChannelInApp:
		return courierv1.Channel_CHANNEL_IN_APP, true
	}
	return courierv1.Channel_CHANNEL_UNSPECIFIED, false
}

// MaxTemplateBytes bounds one part of an edited template (the whole set is loaded in every process).
const MaxTemplateBytes = 64 << 10

// Content is what the admin edits: e-mail has Subject, Text and HTML; Telegram has Text; in-app has Subject (the
// title) and Text (the body).
type Content struct {
	Subject, Text, HTML string
}

// Files turns the content of a channel into the files of its template directory (file name -> text), and checks
// what the channel needs: a subject and a text or html body for e-mail, a text for Telegram, a title and a body in-app.
func Files(channel string, c Content) (map[string]string, error) {
	for _, v := range []string{c.Subject, c.Text, c.HTML} {
		if len(v) > MaxTemplateBytes {
			return nil, fmt.Errorf("a part of a template is limited to %d bytes", MaxTemplateBytes)
		}
	}
	files := map[string]string{}
	put := func(name, v string) {
		if strings.TrimSpace(v) != "" {
			files[name] = v
		}
	}
	// an optional part left empty is stored as an empty file: it hides the built-in one (see Overlay.Open)
	hide := func(name string) {
		if _, ok := files[name]; !ok {
			files[name] = ""
		}
	}
	switch channel {
	case ChannelEmail:
		put("email.subject.tmpl", c.Subject)
		put("email.txt.tmpl", c.Text)
		put("email.html.tmpl", c.HTML)
		if files["email.subject.tmpl"] == "" || (files["email.txt.tmpl"] == "" && files["email.html.tmpl"] == "") {
			return nil, errors.New("an e-mail needs a subject and a text or an html body")
		}
		hide("email.txt.tmpl")
		hide("email.html.tmpl")
	case ChannelTelegram:
		put("telegram.txt.tmpl", c.Text)
		if files["telegram.txt.tmpl"] == "" {
			return nil, errors.New("a Telegram message needs a text")
		}
	case ChannelInApp:
		put("in_app.title.tmpl", c.Subject)
		put("in_app.body.tmpl", c.Text)
		if files["in_app.title.tmpl"] == "" || files["in_app.body.tmpl"] == "" {
			return nil, errors.New("an in-app notification needs a title and a body")
		}
	default:
		return nil, fmt.Errorf("unknown channel %q", channel)
	}
	return files, nil
}

// ContentOf is the inverse of Files.
func ContentOf(channel string, files map[string]string) Content {
	switch channel {
	case ChannelEmail:
		return Content{Subject: files["email.subject.tmpl"], Text: files["email.txt.tmpl"], HTML: files["email.html.tmpl"]}
	case ChannelTelegram:
		return Content{Text: files["telegram.txt.tmpl"]}
	case ChannelInApp:
		return Content{Subject: files["in_app.title.tmpl"], Text: files["in_app.body.tmpl"]}
	}
	return Content{}
}

// channelFiles are the files of a channel, to read a built-in template.
var channelFiles = map[string][]string{
	ChannelEmail:    {"email.subject.tmpl", "email.txt.tmpl", "email.html.tmpl"},
	ChannelTelegram: {"telegram.txt.tmpl"},
	ChannelInApp:    {"in_app.title.tmpl", "in_app.body.tmpl"},
}

// OverlayPaths puts the files of a template under their directory in the file system: locale/template/file.
func OverlayPaths(locale, template string, files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for f, c := range files {
		out[path.Join(NormalizeLocale(locale), template, f)] = c
	}
	return out
}

// Source returns the files of the built-in template of a channel and locale (ignoring any edit); empty when the
// template has none for that locale.
func (r *Renderer) Source(locale, template, channel string) map[string]string {
	out := map[string]string{}
	for _, f := range channelFiles[channel] {
		if b, err := fs.ReadFile(r.base, path.Join(NormalizeLocale(locale), template, f)); err == nil {
			out[f] = string(b)
		}
	}
	return out
}

// Locales lists the locales that have built-in templates.
func (r *Renderer) Locales() []string {
	entries, err := fs.ReadDir(r.base, ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// SetOverrides replaces the edited templates (path -> text); see Overlay.Set.
func (r *Renderer) SetOverrides(files map[string]string) {
	if r.overlay != nil {
		r.overlay.Set(files)
	}
}

// Draft renders a template that is not saved: the given files stand in for the template of the locale, over the
// built-in files (the layout of the e-mails stays). It is how a save is checked and how the preview is made.
func (r *Renderer) Draft(locale, template, channel string, files map[string]string, data map[string]string) (Content, error) {
	ch, ok := ChannelOf(channel)
	if !ok {
		return Content{}, fmt.Errorf("unknown channel %q", channel)
	}
	ov := NewOverlay(r.base)
	ov.Set(OverlayPaths(locale, template, files))
	draft := &Renderer{fs: ov, base: r.base, overlay: ov, defaultLocale: NormalizeLocale(locale), defaults: r.defaults}
	msg, err := draft.Render(locale, template, ch, data)
	if err != nil {
		return Content{}, err
	}
	switch ch {
	case courierv1.Channel_CHANNEL_EMAIL:
		return Content{Subject: msg.Subject, Text: msg.Text, HTML: msg.HTML}, nil
	default:
		return Content{Subject: msg.Subject, Text: msg.Text}, nil
	}
}
