package render

import (
	"os"
	"testing"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// Writes every identity mail to $MAIL_PREVIEW_DIR to look at in a browser; skipped without it.
func TestPreviewMails(t *testing.T) {
	dir := os.Getenv("MAIL_PREVIEW_DIR")
	if dir == "" {
		t.Skip("MAIL_PREVIEW_DIR is not set")
	}
	_ = os.MkdirAll(dir, 0o755)
	r := New("en")
	data := map[string]string{"name": "Hu", "link": "https://auth.example.com/password/set?token=abc123", "expires_in": "7", "change": "reset", "logo_url": os.Getenv("MAIL_PREVIEW_LOGO"),
		"applicant_name": "An Nguyen", "applicant_email": "an@example.com"}
	for _, loc := range []string{"en", "vi"} {
		for _, id := range []string{"identity_welcome", "identity_password_reset", "identity_password_changed", "identity_mfa_changed", "identity_registration_received", "identity_registration_rejected", "identity_registration_pending"} {
			m, err := r.Render(loc, id, courierv1.Channel_CHANNEL_EMAIL, data)
			if err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(dir+"/"+loc+"_"+id+".html", []byte(m.HTML), 0o644)
			_ = os.WriteFile(dir+"/"+loc+"_"+id+".txt", []byte(m.Subject+"\n\n"+m.Text), 0o644)
		}
	}
}
