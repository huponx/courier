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

func TestBvchainTemplates(t *testing.T) {
	r := New("en")
	cases := map[string]map[string]string{
		"bvchain_checkin":         {"shift": "1", "time": "08:31"},
		"bvchain_checkout":        {"shift": "2", "time": "17:42"},
		"bvchain_failed":          {"step": "authentication", "error": "login returned no access token"},
		"bvreport_working_report": {"from_date": "21/09", "to_date": "20/10", "total_in_period": "22", "today": "19/10", "total_to_today": "20", "days_with_cico": "19", "missing_days": "1"},
	}
	for _, loc := range []string{"en", "vi"} {
		for id, data := range cases {
			msg, err := r.Render(loc, id, courierv1.Channel_CHANNEL_TELEGRAM, data)
			if err != nil {
				t.Fatalf("%s/%s: %v", loc, id, err)
			}
			if msg.Text == "" || strings.Contains(msg.Text, "{{") || strings.Contains(msg.Text, "<no value>") {
				t.Fatalf("%s/%s: not rendered: %q", loc, id, msg.Text)
			}
			for _, v := range data {
				if !strings.Contains(msg.Text, v) {
					t.Fatalf("%s/%s: %q missing from %q", loc, id, v, msg.Text)
				}
			}
		}
	}
	// shift 1 is the morning and any other value the afternoon
	if m, _ := r.Render("vi", "bvchain_checkin", courierv1.Channel_CHANNEL_TELEGRAM, map[string]string{"shift": "1", "time": "x"}); !strings.Contains(m.Text, "sáng") {
		t.Fatalf("morning shift: %q", m.Text)
	}
	if m, _ := r.Render("vi", "bvchain_checkin", courierv1.Channel_CHANNEL_TELEGRAM, map[string]string{"shift": "2", "time": "x"}); !strings.Contains(m.Text, "chiều") {
		t.Fatalf("afternoon shift: %q", m.Text)
	}
}
