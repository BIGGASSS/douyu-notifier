package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/BIGGASSS/douyu-notifier/internal/model"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func client(doer HTTPDoer) *Client { return NewClient("TOKEN", "7", doer, time.Second, 60) }

func TestUpdatesPollingConflict(t *testing.T) {
	c := client(doerFunc(func(*http.Request) (*http.Response, error) {
		return response(409, `{"ok":false,"description":"terminated by other getUpdates request"}`), nil
	}))
	c.prepared = true
	_, _, err := c.updates(context.Background(), nil, 60)
	var conflict *PollingConflict
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "already being consumed") {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestPrepareDeletesWebhookOnce(t *testing.T) {
	calls := 0
	c := client(doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/botTOKEN/deleteWebhook" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["drop_pending_updates"] {
			t.Fatal("drop pending true")
		}
		return response(200, `{"ok":true,"result":true}`), nil
	}))
	if err := c.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestUpdatesPassesOffsetAndTimeout(t *testing.T) {
	c := client(doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("offset") != "41" || r.URL.Query().Get("timeout") != "17" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		return response(200, `{"ok":true,"result":[{"update_id":44,"message":{"chat":{"id":7},"text":"hello"}}]}`), nil
	}))
	c.prepared = true
	offset := int64(41)
	updates, next, err := c.updates(context.Background(), &offset, 17)
	if err != nil || len(updates) != 1 || next != 45 {
		t.Fatalf("updates=%#v next=%d err=%v", updates, next, err)
	}
}

func TestProcessPingCommandsAndHealth(t *testing.T) {
	var sent string
	c := client(doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			return response(200, `{"ok":true,"result":[{"update_id":41,"message":{"chat":{"id":99},"text":"/ping"}},{"update_id":42,"edited_message":{"chat":{"id":7},"text":" /ping "}}]}`), nil
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body["text"]
		return response(200, `{"ok":true}`), nil
	}))
	c.prepared = true
	c.startTime = time.Unix(1000, 0)
	c.now = func() time.Time { return time.Unix(1365, 0) }
	c.UpdateHealth(3)
	next, err := c.ProcessPingCommands(context.Background(), 40, 17)
	if err != nil || next != 43 {
		t.Fatalf("next=%d err=%v", next, err)
	}
	for _, want := range []string{"<b>Pong!</b>", "Uptime: 6m 5s", "Last poll: 0s ago", "Live streamers: 3"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("message %q missing %q", sent, want)
		}
	}
}

func TestPingFormatsHoursMinutesAndRecentPolls(t *testing.T) {
	tests := []struct {
		name            string
		started, polled time.Time
		now             time.Time
		live            int
		want            []string
	}{
		{
			name:    "hours and minutes",
			started: time.Unix(1000, 0), polled: time.Unix(7200, 0), now: time.Unix(7261, 0), live: 5,
			want: []string{"Uptime: 1h 44m 21s", "Last poll: 1m ago", "Live streamers: 5"},
		},
		{
			name:    "recent poll in seconds",
			started: time.Unix(1000, 0), polled: time.Unix(1090, 0), now: time.Unix(1100, 0),
			want: []string{"Uptime: 1m 40s", "Last poll: 10s ago", "Live streamers: 0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent string
			c := client(doerFunc(func(r *http.Request) (*http.Response, error) {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				sent = body["text"]
				return response(http.StatusOK, `{"ok":true}`), nil
			}))
			c.startTime, c.lastPollTime, c.hasLastPoll, c.liveCount = tt.started, tt.polled, true, tt.live
			c.now = func() time.Time { return tt.now }
			c.handlePing(context.Background())
			for _, want := range tt.want {
				if !strings.Contains(sent, want) {
					t.Fatalf("message %q missing %q", sent, want)
				}
			}
		})
	}
}

func TestProcessPingIgnoresConflict(t *testing.T) {
	c := client(doerFunc(func(*http.Request) (*http.Response, error) { return response(409, `{}`), nil }))
	c.prepared = true
	offset, err := c.ProcessPingCommands(context.Background(), 17, 0)
	if err != nil || offset != 17 {
		t.Fatalf("offset=%d err=%v", offset, err)
	}
}

func TestProcessNotificationsTransitionsAndEscaping(t *testing.T) {
	var sent []string
	c := client(doerFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = append(sent, body["text"])
		return response(200, `{}`), nil
	}))
	rooms := []model.Room{{RoomID: "dy_1", StreamerName: `A&B<Z>"'`, RoomName: "Room & <fun>", AreaName: "Game", URL: `https://www.douyu.com/1?x=1&label="fun"`, IsLive: true}}
	first := c.ProcessNotifications(context.Background(), rooms, nil)
	if len(first) != 1 || len(sent) != 0 {
		t.Fatalf("first=%#v sent=%#v", first, sent)
	}
	c.ProcessNotifications(context.Background(), rooms, map[string]struct{}{})
	if len(sent) != 1 || !strings.Contains(sent[0], `<b>A&amp;B&lt;Z&gt;&quot;&#x27;</b>`) || !strings.Contains(sent[0], `x=1&amp;label=&quot;fun&quot;`) {
		t.Fatalf("sent=%#v", sent)
	}

	sent = nil
	current := c.ProcessNotifications(context.Background(), []model.Room{{RoomID: "dy_1", StreamerName: "Alice"}, {RoomID: "dy_2", StreamerName: "Bob"}}, map[string]struct{}{"dy_1": {}, "dy_2": {}})
	if len(current) != 0 || len(sent) != 2 {
		t.Fatalf("current=%#v ends=%#v", current, sent)
	}
	joined := strings.Join(sent, "\n")
	for _, want := range []string{"<b>Alice</b> has ended their stream.", "<b>Bob</b> has ended their stream."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ends=%#v missing %q", sent, want)
		}
	}

	sent = nil
	unchanged := c.ProcessNotifications(context.Background(), []model.Room{{RoomID: "dy_1", StreamerName: "Alice", IsLive: true}}, map[string]struct{}{"dy_1": {}})
	if len(unchanged) != 1 || len(sent) != 0 {
		t.Fatalf("unchanged=%#v sent=%#v", unchanged, sent)
	}

	sent = nil
	current = c.ProcessNotifications(context.Background(), []model.Room{{RoomID: "dy_1", StreamerName: "Alice"}, {RoomID: "dy_2", StreamerName: "Bob", IsLive: true}}, map[string]struct{}{"dy_1": {}, "dy_2": {}})
	if _, ok := current["dy_2"]; !ok || len(current) != 1 || len(sent) != 1 || sent[0] != "<b>Alice</b> has ended their stream." {
		t.Fatalf("current=%#v sent=%#v", current, sent)
	}
}

func TestUpdatesKeepsOffsetAfterNonConflictHTTPFailure(t *testing.T) {
	c := client(doerFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusBadGateway, `service unavailable`), nil
	}))
	c.prepared = true
	offset := int64(41)
	updates, next, err := c.updates(context.Background(), &offset, 17)
	if err != nil || len(updates) != 0 || next != offset {
		t.Fatalf("updates=%#v next=%d err=%v", updates, next, err)
	}
}

func TestSendUsesHTMLPayload(t *testing.T) {
	c := NewClient("TOKEN", "123", doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["chat_id"] != "123" || body["parse_mode"] != "HTML" || body["text"] != "<b>hello</b>" {
			t.Fatalf("body=%#v", body)
		}
		return response(200, `{}`), nil
	}), time.Second, 60)
	if !c.Send(context.Background(), "<b>hello</b>") {
		t.Fatal("Send false")
	}
}

func TestWebhookConflictAndEndpoint(t *testing.T) {
	if !strings.Contains(conflictMessage("can't use getUpdates while webhook is active"), "active webhook") {
		t.Fatal("missing webhook guidance")
	}
	c := client(nil)
	if _, err := url.ParseRequestURI(c.endpoint("getUpdates")); err != nil {
		t.Fatal(err)
	}
}
