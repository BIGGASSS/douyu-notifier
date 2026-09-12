package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BIGGASSS/douyu-notifier/internal/model"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// PollingConflict reports that getUpdates cannot be used by this process.
type PollingConflict struct{ Message string }

func (e *PollingConflict) Error() string { return e.Message }

type Client struct {
	token, chatID           string
	http                    HTTPDoer
	grace                   time.Duration
	longPollTimeout         int
	now                     func() time.Time
	sleep                   func(context.Context, time.Duration) error
	mu                      sync.Mutex
	prepared                bool
	healthMu                sync.RWMutex
	startTime, lastPollTime time.Time
	hasLastPoll             bool
	liveCount               int
}

func NewClient(token, chatID string, client HTTPDoer, grace time.Duration, longPollTimeout int) *Client {
	now := time.Now
	return &Client{token: token, chatID: chatID, http: client, grace: grace, longPollTimeout: longPollTimeout, now: now, sleep: sleepContext, startTime: now()}
}

func (c *Client) endpoint(method string) string {
	return "https://api.telegram.org/bot" + c.token + "/" + method
}

type chat struct {
	ID json.RawMessage `json:"id"`
}
type message struct {
	Chat chat   `json:"chat"`
	Text string `json:"text"`
}
type update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *message `json:"message"`
	EditedMessage *message `json:"edited_message"`
}
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description json.RawMessage `json:"description"`
	Result      []update        `json:"result"`
}

func (c *Client) Send(ctx context.Context, text string) bool {
	body, _ := json.Marshal(map[string]string{"chat_id": c.chatID, "text": text, "parse_mode": "HTML"})
	requestContext, cancel := context.WithTimeout(ctx, c.grace)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.endpoint("sendMessage"), bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		var response *http.Response
		response, err = c.http.Do(req)
		if err == nil {
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode >= 200 && response.StatusCode < 400 {
				return true
			}
			err = fmt.Errorf("HTTP status %s", response.Status)
		}
	}
	if ctx.Err() == nil {
		fmt.Printf("Warning: Failed to send Telegram message: %v\n", err)
	}
	return false
}

func (c *Client) prepare(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prepared {
		return nil
	}
	requestContext, cancel := context.WithTimeout(ctx, c.grace)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.endpoint("deleteWebhook"), bytes.NewReader([]byte(`{"drop_pending_updates":false}`)))
	if err != nil {
		fmt.Printf("Warning: Failed to delete Telegram webhook: %v\n", err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Printf("Warning: Failed to delete Telegram webhook: %v\n", err)
		return nil
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Printf("Warning: Failed to delete Telegram webhook: %v\n", err)
		return nil
	}
	description := extractDescription(body)
	if response.StatusCode == http.StatusConflict {
		return &PollingConflict{Message: conflictMessage(description)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		fmt.Printf("Warning: Telegram deleteWebhook failed: %d %s\n", response.StatusCode, description)
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		c.prepared = true
		return nil
	}
	if ok, _ := payload["ok"].(bool); !ok {
		fmt.Printf("Warning: Telegram deleteWebhook returned an error: %s\n", strings.TrimSpace(string(body)))
		return nil
	}
	c.prepared = true
	return nil
}

func (c *Client) updates(ctx context.Context, offset *int64, timeout int) ([]update, int64, error) {
	if err := c.prepare(ctx); err != nil {
		return nil, offsetValue(offset), err
	}
	endpoint, err := url.Parse(c.endpoint("getUpdates"))
	if err != nil {
		return nil, offsetValue(offset), err
	}
	query := endpoint.Query()
	query.Set("timeout", strconv.Itoa(timeout))
	if offset != nil {
		query.Set("offset", strconv.FormatInt(*offset, 10))
	}
	endpoint.RawQuery = query.Encode()
	requestContext, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+c.grace)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, offsetValue(offset), err
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, offsetValue(offset), ctx.Err()
		}
		fmt.Printf("Warning: Failed to fetch Telegram updates: %v\n", err)
		return nil, offsetValue(offset), nil
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Printf("Warning: Failed to fetch Telegram updates: %v\n", err)
		return nil, offsetValue(offset), nil
	}
	description := extractDescription(body)
	if response.StatusCode == http.StatusConflict {
		return nil, offsetValue(offset), &PollingConflict{Message: conflictMessage(description)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		fmt.Printf("Warning: Telegram updates request failed: %d %s\n", response.StatusCode, description)
		return nil, offsetValue(offset), nil
	}
	var payload apiResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, offsetValue(offset), fmt.Errorf("invalid Telegram response: %w", err)
	}
	if !payload.OK {
		fmt.Printf("Warning: Telegram API returned an error: %s\n", strings.TrimSpace(string(body)))
		return nil, offsetValue(offset), nil
	}
	next := offsetValue(offset)
	if len(payload.Result) > 0 {
		next = payload.Result[len(payload.Result)-1].UpdateID + 1
	}
	return payload.Result, next, nil
}

func (c *Client) NextUpdateOffset(ctx context.Context) (int64, error) {
	updates, next, err := c.updates(ctx, nil, 0)
	if err != nil {
		return 0, err
	}
	if len(updates) > 0 {
		return next, nil
	}
	return 0, nil
}

func (c *Client) WaitForChatMessage(ctx context.Context, offset int64) (string, int64, error) {
	current := offset
	for {
		updates, next, err := c.updates(ctx, &current, c.longPollTimeout)
		if err != nil {
			return "", current, err
		}
		current = next
		if len(updates) == 0 {
			if err := c.sleep(ctx, 5*time.Second); err != nil {
				return "", current, err
			}
			continue
		}
		for _, item := range updates {
			message := item.Message
			if message == nil {
				message = item.EditedMessage
			}
			if message != nil && scalar(message.Chat.ID) == c.chatID {
				if text := strings.TrimSpace(message.Text); text != "" {
					return text, current, nil
				}
			}
		}
	}
}

func (c *Client) ProcessPingCommands(ctx context.Context, offset int64, timeout int) (int64, error) {
	updates, next, err := c.updates(ctx, &offset, timeout)
	if err != nil {
		var conflict *PollingConflict
		if errors.As(err, &conflict) {
			return offset, nil
		}
		return offset, err
	}
	for _, item := range updates {
		message := item.Message
		if message == nil {
			message = item.EditedMessage
		}
		if message != nil && scalar(message.Chat.ID) == c.chatID && strings.TrimSpace(message.Text) == "/ping" {
			c.handlePing(ctx)
		}
	}
	return next, nil
}

func (c *Client) UpdateHealth(liveCount int) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.lastPollTime = c.now()
	c.hasLastPoll = true
	c.liveCount = liveCount
}
func (c *Client) handlePing(ctx context.Context) {
	now := c.now()
	c.healthMu.RLock()
	started, polled, hasPoll, live := c.startTime, c.lastPollTime, c.hasLastPoll, c.liveCount
	c.healthMu.RUnlock()
	seconds := int(now.Sub(started).Seconds())
	hours, minutes := seconds/3600, (seconds%3600)/60
	uptime := fmt.Sprintf("%ds", seconds)
	if hours != 0 {
		uptime = fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds%60)
	} else if minutes != 0 {
		uptime = fmt.Sprintf("%dm %ds", minutes, seconds%60)
	}
	lastPoll := "never"
	if hasPoll {
		ago := int(now.Sub(polled).Seconds())
		if ago < 60 {
			lastPoll = fmt.Sprintf("%ds ago", ago)
		} else {
			lastPoll = fmt.Sprintf("%dm ago", ago/60)
		}
	}
	c.Send(ctx, fmt.Sprintf("<b>Pong!</b>\nUptime: %s\nLast poll: %s\nLive streamers: %d", uptime, lastPoll, live))
}

// ProcessNotifications emits live/offline transitions and returns the current live set.
func (c *Client) ProcessNotifications(ctx context.Context, rooms []model.Room, previous map[string]struct{}) map[string]struct{} {
	current := liveIDs(rooms)
	if previous != nil {
		for _, room := range rooms {
			if !room.IsLive {
				continue
			}
			if _, ok := previous[room.RoomID]; ok {
				continue
			}
			c.Send(ctx, fmt.Sprintf("<b>%s</b> is now live!\n%s\nCategory: %s\n<a href=\"%s\">Watch</a>", Escape(room.StreamerName), Escape(room.RoomName), Escape(room.AreaName), Escape(room.URL)))
			fmt.Printf("  Notified: %s is live\n", room.StreamerName)
		}
		lookup := make(map[string]model.Room, len(rooms))
		for _, room := range rooms {
			lookup[room.RoomID] = room
		}
		for id := range previous {
			if _, ok := current[id]; ok {
				continue
			}
			name := Escape(id)
			if room, ok := lookup[id]; ok {
				name = Escape(room.StreamerName)
			}
			c.Send(ctx, fmt.Sprintf("<b>%s</b> has ended their stream.", name))
			fmt.Printf("  Notified: %s ended stream\n", name)
		}
	}
	return current
}

func liveIDs(rooms []model.Room) map[string]struct{} {
	result := map[string]struct{}{}
	for _, room := range rooms {
		if room.IsLive {
			result[room.RoomID] = struct{}{}
		}
	}
	return result
}
func Escape(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	text = strings.ReplaceAll(text, "\"", "&quot;")
	return strings.ReplaceAll(text, "'", "&#x27;")
}
func extractDescription(body []byte) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return strings.TrimSpace(string(body))
	}
	return strings.TrimSpace(scalar(payload["description"]))
}
func conflictMessage(description string) string {
	if strings.Contains(strings.ToLower(description), "webhook") {
		return "Telegram bot polling is blocked by an active webhook. Disable the webhook for this bot token and try again."
	}
	return "Telegram getUpdates is already being consumed by another process for this bot token. Stop the other bot instance or use a different bot token."
}
func scalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return "None"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return trimmed
}
func offsetValue(offset *int64) int64 {
	if offset == nil {
		return 0
	}
	return *offset
}
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
