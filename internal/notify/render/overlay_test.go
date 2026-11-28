package render

import (
	"strings"
	"testing"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// An e-mail saved without an html part must not send the built-in html with the new text.
func TestClearedHTMLPartDoesNotComeBack(t *testing.T) {
	r := New("en")
	builtIn := r.Source("en", "hello", ChannelEmail)
	if builtIn["email.html.tmpl"] == "" {
		t.Skip("the hello template has no html part any more")
	}
	files, err := Files(ChannelEmail, Content{Subject: "Hi {{.name}}", Text: "plain only"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Draft("en", "hello", ChannelEmail, files, map[string]string{"name": "An"})
	if err != nil {
		t.Fatal(err)
	}
	if got.HTML != "" || !strings.Contains(got.Text, "plain only") || got.Subject != "Hi An" {
		t.Fatalf("%+v", got)
	}
	// saved and loaded through the overlay of a running renderer, the same
	r.SetOverrides(OverlayPaths("en", "hello", files))
	live, err := r.Render("en", "hello", courierv1.Channel_CHANNEL_EMAIL, map[string]string{"name": "An"})
	if err != nil || live.HTML != "" {
		t.Fatalf("%+v %v", live, err)
	}
	// reverted: the built-in html is back
	r.SetOverrides(nil)
	back, _ := r.Render("en", "hello", courierv1.Channel_CHANNEL_EMAIL, map[string]string{"name": "An"})
	if back.HTML == "" {
		t.Fatal("the built-in html must be back after a revert")
	}
}

func TestFilesRefusesAnOversizedPart(t *testing.T) {
	if _, err := Files(ChannelTelegram, Content{Text: strings.Repeat("x", MaxTemplateBytes+1)}); err == nil {
		t.Fatal("accepted")
	}
	if _, err := Files(ChannelTelegram, Content{Text: strings.Repeat("x", MaxTemplateBytes)}); err != nil {
		t.Fatal(err)
	}
}

// a value the caller did not send is empty, never the text "<no value>" in a mail
func TestMissingValueIsEmptyNotNoValue(t *testing.T) {
	r := New("en")
	files, _ := Files(ChannelEmail, Content{Subject: "Hi {{.name}}!", Text: "Hello {{.name}},\nbye", HTML: "<p>{{.name}}</p>"})
	got, err := r.Draft("en", "hello", ChannelEmail, files, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{got.Subject, got.Text, got.HTML} {
		if strings.Contains(part, "no value") {
			t.Fatalf("%q", part)
		}
	}
	if got.Subject != "Hi !" {
		t.Fatalf("%q", got.Subject)
	}
}
