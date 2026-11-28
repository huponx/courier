package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/huponx/gocommon/netsafe"
)

// Binder links a chat to the user that owns a code (store.ConsumeBindingCode); ErrInvalid says the code is unknown or expired.
type Binder interface {
	Bind(ctx context.Context, code, chatID string) error
}

// Poller listens to the bot with getUpdates and answers `/start <code>`: the user opened the link of the portal,
// the code proves the chat is theirs. Only one process may poll a bot at a time (Telegram answers 409 to a second
// one): the caller runs it under a lock.
type Poller struct {
	Token string
	// BaseURL defaults to https://api.telegram.org.
	BaseURL string
	HTTP    *http.Client
	Binder  Binder
	// IsInvalid tells a Bind error that means "unknown or expired code" (the user gets a message, nothing is logged).
	IsInvalid func(error) bool
	Log       func(msg string, err error)
	// Wait is the long poll timeout in seconds (default 25).
	Wait int

	offset int64
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		From *struct {
			Language string `json:"language_code"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
}

func (p *Poller) base() string {
	if p.BaseURL != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return "https://api.telegram.org"
}

func (p *Poller) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (p *Poller) call(ctx context.Context, method string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base()+"/bot"+p.Token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return netsafe.Scrub(err) // the URL of the request holds the token of the bot
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: status %d", method, resp.StatusCode)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Run polls until ctx is done or Telegram fails (for example 409: another poller): the caller decides when to retry.
func (p *Poller) Run(ctx context.Context) error {
	wait := p.Wait
	if wait <= 0 {
		wait = 25
	}
	for ctx.Err() == nil {
		var updates []update
		err := p.call(ctx, "getUpdates", map[string]any{"offset": p.offset, "timeout": wait, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutting down
			}
			return err
		}
		for _, u := range updates {
			p.offset = u.UpdateID + 1
			p.handle(ctx, u)
		}
	}
	return nil
}

func (p *Poller) handle(ctx context.Context, u update) {
	m := u.Message
	if m == nil || m.Chat.Type != "private" || !strings.HasPrefix(m.Text, "/start") {
		return
	}
	vi := m.From != nil && strings.HasPrefix(strings.ToLower(m.From.Language), "vi")
	text := func(en, viText string) string {
		if vi {
			return viText
		}
		return en
	}
	code := strings.TrimSpace(strings.TrimPrefix(m.Text, "/start"))
	chat := fmt.Sprint(m.Chat.ID)
	reply := ""
	switch {
	case code == "":
		reply = text("Open the link in your account settings (Notifications, Link Telegram) to connect this chat.",
			"Hãy mở liên kết trong phần cài đặt tài khoản (Thông báo, Liên kết Telegram) để kết nối cuộc trò chuyện này.")
	default:
		if err := p.Binder.Bind(ctx, strings.ToUpper(code), chat); err != nil {
			if p.IsInvalid != nil && p.IsInvalid(err) {
				reply = text("This code is not valid or has expired. Create a new one in your account settings.",
					"Mã này không hợp lệ hoặc đã hết hạn. Hãy tạo mã mới trong cài đặt tài khoản.")
			} else {
				if p.Log != nil {
					p.Log("telegram bind", err)
				}
				return // no answer: the user can send the link again
			}
		} else {
			reply = text("Linked. You will get your notifications here.", "Đã liên kết. Thông báo của bạn sẽ được gửi vào đây.")
		}
	}
	if err := p.call(ctx, "sendMessage", map[string]any{"chat_id": m.Chat.ID, "text": reply}, nil); err != nil && p.Log != nil {
		p.Log("telegram reply", err)
	}
}
