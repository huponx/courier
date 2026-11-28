package email

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestSubjectCannotInjectHeadersAndIsEncoded(t *testing.T) {
	raw, err := buildMIME("a@x.io", "b@x.io", notify.Message{Subject: "Đăng ký\rBcc: evil@x.io\nX: y", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.SplitN(string(raw), "\r\n\r\n", 2)[0]
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") || strings.HasPrefix(line, "X:") {
			t.Fatalf("a header was injected: %q", head)
		}
	}
	if !strings.Contains(head, "Subject: =?UTF-8?q?") {
		t.Fatalf("a non-ASCII subject must be an encoded word: %q", head)
	}
	if _, err := buildMIME("a@x.io", "b@x.io\r\nBcc: evil@x.io", notify.Message{Text: "hi"}); err == nil || !notify.IsPermanent(err) {
		t.Fatalf("an address with a line break: %v", err)
	}
}

// a server that accepts the connection and says nothing must not hold the send for ever
func TestStalledServerEndsWithTheContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // never writes the 220 greeting
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portText)
	s := New(Config{Host: host, Port: port, From: "a@x.io"})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = s.Send(ctx, notify.Destination{Address: "b@x.io"}, notify.Message{Subject: "s", Text: "t"})
	if err == nil || time.Since(began) > 5*time.Second {
		t.Fatalf("took %s, err %v", time.Since(began), err)
	}
}

// fakeSMTP accepts one mail and returns the MAIL FROM and RCPT TO lines it saw.
func fakeSMTP(t *testing.T) (host string, port int, lines <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 8)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		reply := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		reply("220 fake")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case inData:
				if line == "." {
					inData = false
					reply("250 queued")
				}
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				reply("250 fake")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				got <- line
				reply("250 ok")
			case line == "DATA":
				inData = true
				reply("354 go")
			case line == "QUIT":
				reply("221 bye")
				return
			default:
				reply("250 ok")
			}
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(portText)
	return host, port, got
}

func TestEnvelopeTakesTheBareAddress(t *testing.T) {
	host, port, lines := fakeSMTP(t)
	// SMTP_FROM with a display name is what production has; the server must see only the address
	s := New(Config{Host: host, Port: port, From: "MMFamily <home@x.io>"})
	err := s.Send(context.Background(), notify.Destination{Address: "Ann <ann@x.io>"}, notify.Message{Subject: "s", Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if from, rcpt := <-lines, <-lines; from != "MAIL FROM:<home@x.io>" || rcpt != "RCPT TO:<ann@x.io>" {
		t.Fatalf("envelope %q %q", from, rcpt)
	}
}

func TestRecipientThatIsNoAddressIsPermanent(t *testing.T) {
	s := New(Config{Host: "127.0.0.1", Port: 1, From: "a@x.io"})
	err := s.Send(context.Background(), notify.Destination{Address: "not an address"}, notify.Message{Subject: "s", Text: "t"})
	if err == nil || !notify.IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
}
