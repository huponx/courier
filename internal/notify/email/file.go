package email

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/huponx/courier/internal/notify"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// FileSender writes every mail to a directory instead of sending it (local runs: MAIL_DIR). Each mail becomes
// <UTC timestamp>_<to>_<subject slug>.html (the HTML part) with the plain text next to it as .txt.
type FileSender struct {
	dir string
}

// NewFile returns a sender that writes into dir; the directory is created on the first mail.
func NewFile(dir string) *FileSender { return &FileSender{dir: dir} }

func (s *FileSender) ID() courierv1.Channel { return courierv1.Channel_CHANNEL_EMAIL }

var unsafeName = regexp.MustCompile(`[^a-z0-9@.+-]+`)

func slug(v string, max int) string {
	v = strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(v), "-"), "-.")
	if len(v) > max {
		v = strings.TrimRight(v[:max], "-.")
	}
	if v == "" {
		return "mail"
	}
	return v
}

func (s *FileSender) Send(_ context.Context, dest notify.Destination, msg notify.Message) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mail dir: %w", err)
	}
	base := fmt.Sprintf("%s_%s_%s", time.Now().UTC().Format("20060102T150405.000000Z"), slug(dest.Address, 60), slug(msg.Subject, 50))
	html := msg.HTML
	if html == "" {
		html = "<pre>" + msg.Text + "</pre>"
	}
	page := "<!doctype html><meta charset=\"utf-8\"><title>" + msg.Subject + "</title>\n<!-- to: " + dest.Address + " -->\n" + html
	if err := os.WriteFile(filepath.Join(s.dir, base+".html"), []byte(page), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, base+".txt"), []byte("To: "+dest.Address+"\nSubject: "+msg.Subject+"\n\n"+msg.Text), 0o644)
}
