package render

import (
	"strings"
	"testing"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

func TestHelloTemplates(t *testing.T) {
	r := New("en")
	data := map[string]string{"name": "Hu"}

	if !r.Has("vi", "hello", courierv1.Channel_CHANNEL_EMAIL) {
		t.Fatal("expected vi/hello/email")
	}
	msg, err := r.Render("vi", "hello", courierv1.Channel_CHANNEL_TELEGRAM, data)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Text == "" || msg.Text == "Xin chào {{.name}}, đây là thông báo thử từ courier." {
		t.Fatalf("template not executed: %q", msg.Text)
	}

	fallback, err := r.Render("fr", "hello", courierv1.Channel_CHANNEL_EMAIL, data)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Subject == "" || fallback.HTML == "" {
		t.Fatalf("fallback render empty: %+v", fallback)
	}
}

func TestIdentityTemplates(t *testing.T) {
	r := New("en")
	cases := map[string]map[string]string{
		"identity_password_reset":   {"name": "Hu", "link": "https://auth.example.com/password/reset?token=abc", "expires_in": "30"},
		"identity_password_changed": {"name": "Hu"},
		"identity_mfa_changed":      {"name": "Hu", "change": "reset"},
	}
	for _, loc := range []string{"en", "vi"} {
		for id, data := range cases {
			msg, err := r.Render(loc, id, courierv1.Channel_CHANNEL_EMAIL, data)
			if err != nil {
				t.Fatalf("%s/%s: %v", loc, id, err)
			}
			if msg.Subject == "" || msg.Text == "" || msg.HTML == "" {
				t.Fatalf("%s/%s: empty part: %+v", loc, id, msg)
			}
			if strings.Contains(msg.Text, "{{") || strings.Contains(msg.HTML, "<no value>") {
				t.Fatalf("%s/%s: not rendered: %q", loc, id, msg.Text)
			}
			if id == "identity_password_reset" && (!strings.Contains(msg.Text, data["link"]) || !strings.Contains(msg.HTML, data["link"])) {
				t.Fatalf("%s: link missing", loc)
			}
		}
	}
}
