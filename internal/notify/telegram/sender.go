package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/gocommon/netsafe"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

type Sender struct {
	token      string
	httpClient *http.Client
}

func New(token string) *Sender {
	return &Sender{token: token, httpClient: &http.Client{Timeout: 15 * time.Second}}
}

func (s *Sender) ID() courierv1.Channel {
	return courierv1.Channel_CHANNEL_TELEGRAM
}

func (s *Sender) Send(ctx context.Context, dest notify.Destination, msg notify.Message) error {
	if s.token == "" {
		return fmt.Errorf("telegram bot token is not configured")
	}
	if dest.Address == "" {
		return fmt.Errorf("telegram chat_id is required")
	}
	if msg.Text == "" {
		return fmt.Errorf("telegram text is empty")
	}

	payload, err := json.Marshal(map[string]string{
		"chat_id": dest.Address,
		"text":    msg.Text,
	})
	if err != nil {
		return err
	}

	url := "https://api.telegram.org/bot" + s.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return netsafe.Scrub(err) // the URL of the request holds the token of the bot
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &parsed)
	if resp.StatusCode >= 300 || !parsed.OK {
		err := fmt.Errorf("telegram: status %d", resp.StatusCode)
		if parsed.Description != "" {
			err = fmt.Errorf("telegram: %s", parsed.Description)
		}
		// 400 (chat not found, bad request) and 403 (bot blocked, user deactivated) never succeed on a retry;
		// 429 and 5xx do.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden {
			// 403: the user blocked the bot or deactivated the account; 400 "chat not found": the chat is gone
			if resp.StatusCode == http.StatusForbidden || strings.Contains(strings.ToLower(parsed.Description), "chat not found") {
				err = fmt.Errorf("%w: %w", notify.ErrRecipientGone, err)
			}
			return notify.Permanent(err)
		}
		return err
	}
	return nil
}
