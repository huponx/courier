package email

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"

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

func (s *Sender) Send(_ context.Context, dest notify.Destination, msg notify.Message) error {
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

	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprintf("%d", s.cfg.Port))
	var auth smtp.Auth
	if s.cfg.User != "" {
		auth = smtp.PlainAuth("", s.cfg.User, s.cfg.Password, s.cfg.Host)
	}
	return smtp.SendMail(addr, auth, s.cfg.From, []string{dest.Address}, body)
}

func buildMIME(from, to string, msg notify.Message) ([]byte, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	fmt.Fprintf(&buf, "To: %s\r\n", to)
	fmt.Fprintf(&buf, "Subject: %s\r\n", strings.ReplaceAll(msg.Subject, "\n", " "))
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
