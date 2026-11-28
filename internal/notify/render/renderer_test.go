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
		"identity_password_reset":        {"name": "Hu", "link": "https://auth.example.com/password/reset?token=abc", "expires_in": "30"},
		"identity_welcome":               {"name": "Hu", "link": "https://auth.example.com/password/reset?token=abc", "expires_in": "7"},
		"identity_password_changed":      {"name": "Hu"},
		"identity_mfa_changed":           {"name": "Hu", "change": "reset"},
		"identity_registration_received": {"name": "Hu"},
		"identity_registration_rejected": {"name": "Hu"},
		"identity_registration_pending":  {"name": "Ad", "applicant_name": "Hu", "applicant_email": "hu@example.com", "link": "https://admin.example.com/users?status=awaiting_approval"},
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
			if !strings.Contains(msg.HTML, "<!doctype html>") || !strings.Contains(msg.HTML, "MM") {
				t.Fatalf("%s/%s: not set into the layout", loc, id)
			}
			if id == "identity_registration_pending" {
				if !strings.Contains(msg.Text, "hu@example.com") || !strings.Contains(msg.HTML, data["link"]) {
					t.Fatalf("%s: applicant or link missing", loc)
				}
				noLink, err := r.Render(loc, id, courierv1.Channel_CHANNEL_EMAIL, map[string]string{"name": "Ad", "applicant_name": "Hu", "applicant_email": "hu@example.com"})
				if err != nil || strings.Contains(noLink.HTML, "href") || strings.Contains(noLink.Text, "http") {
					t.Fatalf("%s: without link: %v %+v", loc, err, noLink)
				}
			}
			if (id == "identity_password_reset" || id == "identity_welcome") && (!strings.Contains(msg.Text, data["link"]) || !strings.Contains(msg.HTML, data["link"])) {
				t.Fatalf("%s: link missing", loc)
			}
		}
	}
}

func TestBrandDefaultsAndCallerOverride(t *testing.T) {
	r := New("en")
	r.SetDefaults(map[string]string{"brand": "Acme", "logo_url": "https://acme.example/logo.png"})
	render := func(data map[string]string) string {
		m, err := r.Render("en", "identity_password_changed", courierv1.Channel_CHANNEL_EMAIL, data)
		if err != nil {
			t.Fatal(err)
		}
		return m.HTML
	}
	if h := render(map[string]string{"name": "Hu"}); !strings.Contains(h, "Acme") || !strings.Contains(h, "https://acme.example/logo.png") {
		t.Errorf("defaults not used: %s", h)
	}
	h := render(map[string]string{"name": "Hu", "brand": "Other", "logo_url": "https://other.example/l.png"})
	if !strings.Contains(h, "Other") || !strings.Contains(h, "https://other.example/l.png") || strings.Contains(h, "Acme") {
		t.Errorf("caller does not override: %s", h)
	}
	if h := render(map[string]string{"name": "Hu", "brand": ""}); !strings.Contains(h, "Acme") {
		t.Errorf("an empty brand from the caller hides the default: %s", h)
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

func TestPasskeyMails(t *testing.T) {
	r := New("en")
	for _, loc := range []string{"en", "vi"} {
		for _, change := range []string{"passkey_added", "passkey_removed", "recovery_regenerated"} {
			msg, err := r.Render(loc, "identity_mfa_changed", courierv1.Channel_CHANNEL_EMAIL, map[string]string{"name": "Hu", "change": change})
			if err != nil {
				t.Fatalf("%s/%s: %v", loc, change, err)
			}
			if strings.Contains(msg.Subject+msg.Text, "changed:") || strings.Contains(msg.Subject, "Two-step") || strings.Contains(msg.Subject, "Xác thực hai bước") {
				t.Errorf("%s/%s fell through to the generic text: %q", loc, change, msg.Subject)
			}
		}
	}
}

func TestRegistrationDigestMail(t *testing.T) {
	r := New("en")
	for _, locale := range []string{"en", "vi"} {
		msg, err := r.Render(locale, "identity_registration_pending_digest", courierv1.Channel_CHANNEL_EMAIL, map[string]string{
			"name": "Hu", "count": "2", "link": "https://admin.example.com/registrations",
			"items": "Ann <ann@example.com> registered\nBob registered",
		})
		if err != nil {
			t.Fatal(locale, err)
		}
		// every event is its own row, escaped on its own, inside the layout of the other mails
		for _, want := range []string{"Ann &lt;ann@example.com&gt; registered</td>", ">Bob registered</td>", `width="20"`, "https://admin.example.com/registrations", "<html"} {
			if !strings.Contains(msg.HTML, want) {
				t.Errorf("%s: html lacks %q:\n%s", locale, want, msg.HTML)
			}
		}
		if strings.Contains(msg.HTML, "<ann@example.com>") {
			t.Errorf("%s: an item was not escaped", locale)
		}
	}
}
