package email

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huponx/courier/internal/notify"
)

func TestFileSenderWritesHTMLAndText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mails")
	s := NewFile(dir)
	err := s.Send(context.Background(), notify.Destination{Address: "A@Example.com"},
		notify.Message{Subject: "Welcome, set your password", Text: "plain link", HTML: "<p>html link</p>"})
	if err != nil {
		t.Fatal(err)
	}
	htmls, _ := filepath.Glob(filepath.Join(dir, "*_a@example.com_welcome-set-your-password.html"))
	txts, _ := filepath.Glob(filepath.Join(dir, "*_a@example.com_welcome-set-your-password.txt"))
	if len(htmls) != 1 || len(txts) != 1 {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("files: %v", entries)
	}
	if b, _ := os.ReadFile(htmls[0]); !strings.Contains(string(b), "<p>html link</p>") {
		t.Errorf("html: %s", b)
	}
	if b, _ := os.ReadFile(txts[0]); !strings.Contains(string(b), "plain link") {
		t.Errorf("txt: %s", b)
	}
}
