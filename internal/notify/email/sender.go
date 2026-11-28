package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/huponx/courier/internal/notify"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
}

type Sender struct {
	cfg Config
}

func New(cfg Config) *Sender {
	return &Sender{cfg: cfg}
}

func (s *Sender) ID() courierv1.Channel {
	return courierv1.Channel_CHANNEL_EMAIL
}

func (s *Sender) Send(ctx context.Context, dest notify.Destination, msg notify.Message) error {
	if s.cfg.Host == "" || s.cfg.From == "" {
		return fmt.Errorf("smtp is not configured")
	}
	if dest.Address == "" {
		return fmt.Errorf("email address is required")
	}
	if msg.Text == "" && msg.HTML == "" {
		return fmt.Errorf("email body is empty")
	}

	body, err := buildMIME(s.cfg.From, dest.Address, msg)
	if err != nil {
		return err
	}
	// the envelope takes the bare address: SMTP_FROM may be "Name <addr>" (the header keeps the name), and a
	// recipient that is not an address is refused here instead of by the server
	from, err := envelopeAddr(s.cfg.From)
	if err != nil {
		return fmt.Errorf("SMTP_FROM: %w", err)
	}
	to, err := envelopeAddr(dest.Address)
	if err != nil {
		return notify.Permanent(fmt.Errorf("recipient: %w", err))
	}

	return classify(s.deliver(ctx, from, to, body))
}

// ioTimeout bounds the whole conversation with the SMTP server: smtp.SendMail has no deadline, so a server that accepts
// the connection and then stalls would hold the send (and its goroutine) for ever.
const ioTimeout = 60 * time.Second

// deliver is smtp.SendMail with a dial timeout and a deadline on the connection (it also follows the context).
func (s *Sender) deliver(ctx context.Context, from, to string, body []byte) error {
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprintf("%d", s.cfg.Port))
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(ioTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if s.cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.User, s.cfg.Password, s.cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// envelopeAddr is the bare address of "addr" or "Name <addr>", what MAIL FROM and RCPT TO take.
func envelopeAddr(v string) (string, error) {
	a, err := mail.ParseAddress(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("not an e-mail address: %w", err)
	}
	return a.Address, nil
}

// classify marks a 5xx reply of the SMTP server (mailbox unavailable, bad address) as permanent;
// 4xx replies and network errors stay retryable.
func classify(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return notify.Permanent(err)
	}
	return err
}

// oneLine refuses a header value with a line break: it would start a header of the attacker's choice.
func oneLine(v string) bool { return !strings.ContainsAny(v, "\r\n") }

func buildMIME(from, to string, msg notify.Message) ([]byte, error) {
	if !oneLine(from) || !oneLine(to) {
		return nil, notify.Permanent(fmt.Errorf("a header value has a line break"))
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	// a subject is text from the templates and may hold the name an applicant typed: no line break, and the
	// non-ASCII letters (Vietnamese) as an RFC 2047 encoded word
	subject := strings.Join(strings.FieldsFunc(msg.Subject, func(r rune) bool { return r == '\r' || r == '\n' }), " ")
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")

	switch {
	case msg.Text != "" && msg.HTML != "":
		var alt bytes.Buffer
		w := multipart.NewWriter(&alt)
		tw, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain; charset=UTF-8"}})
		if err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(msg.Text)); err != nil {
			return nil, err
		}
		hw, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/html; charset=UTF-8"}})
		if err != nil {
			return nil, err
		}
		if _, err := hw.Write([]byte(msg.HTML)); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", w.Boundary())
		buf.Write(alt.Bytes())
	case msg.HTML != "":
		buf.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
		buf.WriteString(msg.HTML)
	default:
		buf.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
		buf.WriteString(msg.Text)
	}
	return buf.Bytes(), nil
}
