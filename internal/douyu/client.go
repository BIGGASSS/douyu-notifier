package douyu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/BIGGASSS/douyu-notifier/internal/model"
)

// HTTPDoer is implemented by http.Client.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// NotLoginError reports that Douyu rejected the current cookies.
type NotLoginError struct{ Message string }

func (e *NotLoginError) Error() string { return e.Message }

// APIError reports a temporary or non-authentication Douyu API failure.
type APIError struct{ Message string }

func (e *APIError) Error() string { return e.Message }

type Client struct {
	endpoint string
	http     HTTPDoer
}

func NewClient(endpoint string, client HTTPDoer) *Client {
	return &Client{endpoint: endpoint, http: client}
}

type apiResponse struct {
	Message json.RawMessage `json:"msg"`
	Error   json.RawMessage `json:"error"`
	Data    struct {
		Rooms []map[string]json.RawMessage `json:"list"`
	} `json:"data"`
}

func (c *Client) Fetch(ctx context.Context, cookies map[string]string) ([]model.Room, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, &APIError{Message: fmt.Sprintf("Request failed: %v", err)}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://www.douyu.com/")
	req.Header.Set("Cookie", cookieHeader(cookies))
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &APIError{Message: fmt.Sprintf("Request failed: %v", err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, &APIError{Message: fmt.Sprintf("Request failed: %v", err)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return nil, &APIError{Message: fmt.Sprintf("Request failed: HTTP status %s", response.Status)}
	}
	var payload apiResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &APIError{Message: fmt.Sprintf("Request failed: invalid JSON response: %v", err)}
	}
	message := rawText(payload.Message, "")
	errorCode := rawScalarString(payload.Error, "None")
	if errorCode == "-1" && strings.Contains(message, "未登") {
		return nil, &NotLoginError{Message: message}
	}
	if errorCode != "0" {
		detail := message
		if detail == "" {
			detail = strings.TrimSpace(string(body))
		}
		return nil, &APIError{Message: fmt.Sprintf("API error: %s", detail)}
	}
	return parseResponse(payload), nil
}

func cookieHeader(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, "; ")
}

func parseResponse(payload apiResponse) []model.Room {
	rooms := make([]model.Room, 0, len(payload.Data.Rooms))
	for _, data := range payload.Data.Rooms {
		roomID := rawScalarString(data["room_id"], "")
		path := "/" + roomID
		if raw, ok := data["url"]; ok {
			path = rawText(raw, "")
		}
		rooms = append(rooms, model.Room{
			RoomID: "dy_" + roomID, RoomName: rawText(data["room_name"], ""),
			StreamerName: rawText(data["nickname"], ""), Cover: rawText(data["room_src"], ""),
			Avatar:   rawText(data["avatar_small"], ""),
			IsLive:   rawNumber(data["show_status"], 0) == 1 && rawNumber(data["videoLoop"], 0) == 0,
			AreaName: rawText(data["game_name"], ""), URL: "https://www.douyu.com" + path, Platform: "douyu",
		})
	}
	return rooms
}

func rawText(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return rawScalarString(raw, fallback)
}
func rawScalarString(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "null":
		return "None"
	case "true":
		return "True"
	case "false":
		return "False"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return trimmed
}
func rawNumber(raw json.RawMessage, fallback float64) float64 {
	if len(raw) == 0 {
		return fallback
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "true" {
		return 1
	}
	if trimmed == "false" {
		return 0
	}
	number, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return fallback
	}
	return number
}
