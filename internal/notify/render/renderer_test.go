package render

import (
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
