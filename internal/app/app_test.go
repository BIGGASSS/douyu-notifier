package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BIGGASSS/douyu-notifier/internal/cookies"
	"github.com/BIGGASSS/douyu-notifier/internal/douyu"
	"github.com/BIGGASSS/douyu-notifier/internal/model"
	"github.com/BIGGASSS/douyu-notifier/internal/telegram"
)

type providerFunc func(context.Context, map[string]string) ([]model.Room, error)

func (f providerFunc) Fetch(ctx context.Context, values map[string]string) ([]model.Room, error) {
	return f(ctx, values)
}

type memoryStore struct {
	values map[string]string
}

func (s *memoryStore) Load() map[string]string       { return s.values }
func (s *memoryStore) Save(values map[string]string) { s.values = values }

type fakeNotifier struct {
	sent         []string
	nextOffset   int64
	nextErr      error
	message      string
	wait         func(context.Context, int64) (string, int64, error)
	ping         func(context.Context, int64, int) (int64, error)
	previousArgs []map[string]struct{}
	roomArgs     [][]model.Room
}

func (n *fakeNotifier) Send(_ context.Context, text string) bool {
	n.sent = append(n.sent, text)
	return true
}
func (n *fakeNotifier) NextUpdateOffset(context.Context) (int64, error) {
	return n.nextOffset, n.nextErr
}
func (n *fakeNotifier) WaitForChatMessage(ctx context.Context, offset int64) (string, int64, error) {
	if n.wait != nil {
		return n.wait(ctx, offset)
	}
	return n.message, n.nextOffset + 1, nil
}
func (n *fakeNotifier) ProcessPingCommands(ctx context.Context, offset int64, timeout int) (int64, error) {
	if n.ping != nil {
		return n.ping(ctx, offset, timeout)
	}
	return offset, nil
}
func (n *fakeNotifier) UpdateHealth(int) {}
func (n *fakeNotifier) ProcessNotifications(_ context.Context, rooms []model.Room, previous map[string]struct{}) map[string]struct{} {
	n.previousArgs = append(n.previousArgs, previous)
	n.roomArgs = append(n.roomArgs, append([]model.Room(nil), rooms...))
	current := map[string]struct{}{}
	for _, room := range rooms {
		if room.IsLive {
			current[room.RoomID] = struct{}{}
		}
	}
	return current
}

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func telegramResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testConfig() Config {
	return Config{BotToken: "TOKEN", ChatID: "123", PollInterval: 70 * time.Second, ValidationRetryDelay: time.Second, ValidationRetries: 3, TelegramLongPollTimeout: 60}
}
func room(id string, live bool) model.Room {
	return model.Room{RoomID: id, StreamerName: "Streamer", IsLive: live}
}

func TestValidateCookiesRetriesAPIError(t *testing.T) {
	calls, sleeps := 0, 0
	a := New(testConfig(), providerFunc(func(context.Context, map[string]string) ([]model.Room, error) {
		calls++
		if calls == 1 {
			return nil, &douyu.APIError{Message: "temporary"}
		}
		return []model.Room{room("dy_1", true)}, nil
	}), &memoryStore{}, &fakeNotifier{}, &bytes.Buffer{})
	a.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	rooms, err := a.validateCookies(context.Background(), map[string]string{"a": "b"})
	if err != nil || calls != 2 || sleeps != 1 || !reflect.DeepEqual(rooms, []model.Room{room("dy_1", true)}) {
		t.Fatalf("rooms=%#v calls=%d sleeps=%d err=%v", rooms, calls, sleeps, err)
	}
}

func TestValidateCookiesDoesNotRetryAuthAndStopsAtLimit(t *testing.T) {
	t.Run("auth", func(t *testing.T) {
		calls := 0
		a := New(testConfig(), providerFunc(func(context.Context, map[string]string) ([]model.Room, error) {
			calls++
			return nil, &douyu.NotLoginError{Message: "expired"}
		}), &memoryStore{}, &fakeNotifier{}, &bytes.Buffer{})
		a.sleep = func(context.Context, time.Duration) error { t.Fatal("slept"); return nil }
		_, err := a.validateCookies(context.Background(), nil)
		var target *douyu.NotLoginError
		if !errors.As(err, &target) || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
	t.Run("limit", func(t *testing.T) {
		calls, sleeps := 0, 0
		a := New(testConfig(), providerFunc(func(context.Context, map[string]string) ([]model.Room, error) {
			calls++
			return nil, &douyu.APIError{Message: "down"}
		}), &memoryStore{}, &fakeNotifier{}, &bytes.Buffer{})
		a.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
		_, err := a.validateCookies(context.Background(), nil)
		var target *douyu.APIError
		if !errors.As(err, &target) || calls != 3 || sleeps != 2 {
			t.Fatalf("calls=%d sleeps=%d err=%v", calls, sleeps, err)
		}
	})
}

func TestWaitWithPingChecksTimeoutsAndCancellation(t *testing.T) {
	n := &fakeNotifier{}
	a := New(testConfig(), nil, nil, n, &bytes.Buffer{})
	times := []time.Time{time.Unix(100, 0), time.Unix(100, 0), time.Unix(130, 0), time.Unix(171, 0)}
	index := 0
	a.now = func() time.Time { value := times[index]; index++; return value }
	type call struct {
		offset  int64
		timeout int
	}
	var calls []call
	n.ping = func(_ context.Context, offset int64, timeout int) (int64, error) {
		calls = append(calls, call{offset, timeout})
		return offset + 1, nil
	}
	offset, err := a.waitWithPingChecks(context.Background(), 70*time.Second, 19)
	want := []call{{19, 60}, {20, 40}}
	if err != nil || offset != 21 || !reflect.DeepEqual(calls, want) {
		t.Fatalf("offset=%d calls=%#v err=%v", offset, calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.now = time.Now
	offset, err = a.waitWithPingChecks(ctx, time.Minute, 12)
	if !errors.Is(err, context.Canceled) || offset != 12 {
		t.Fatalf("cancel offset=%d err=%v", offset, err)
	}
}

func TestWaitWithPingChecksUsesShortRemainingTimeout(t *testing.T) {
	n := &fakeNotifier{}
	a := New(testConfig(), nil, nil, n, &bytes.Buffer{})
	times := []time.Time{time.Unix(100, 0), time.Unix(100, 0), time.Unix(104, 200_000_000)}
	index := 0
	a.now = func() time.Time {
		value := times[index]
		index++
		return value
	}
	var gotOffset int64
	var gotTimeout int
	n.ping = func(_ context.Context, offset int64, timeout int) (int64, error) {
		gotOffset, gotTimeout = offset, timeout
		return 55, nil
	}
	offset, err := a.waitWithPingChecks(context.Background(), 4*time.Second, 12)
	if err != nil || offset != 55 || gotOffset != 12 || gotTimeout != 4 {
		t.Fatalf("offset=%d err=%v ping=(%d,%d)", offset, err, gotOffset, gotTimeout)
	}
}

func TestRecoverCookiesValidatesSavesAndSkipsOldTelegramUpdates(t *testing.T) {
	expected := map[string]string{"acf_uid": "1", "dy_did": "2"}
	store := cookies.NewStore(filepath.Join(t.TempDir(), "cookies.json"), io.Discard)
	updatesCalls := 0
	var sent []string
	notifier := telegram.NewClient("TOKEN", "123", httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/deleteWebhook"):
			return telegramResponse(http.StatusOK, `{"ok":true}`), nil
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			updatesCalls++
			if updatesCalls == 1 {
				if request.URL.Query().Get("offset") != "" || request.URL.Query().Get("timeout") != "0" {
					t.Fatalf("initial query=%q", request.URL.RawQuery)
				}
				return telegramResponse(http.StatusOK, `{"ok":true,"result":[{"update_id":5,"message":{"chat":{"id":123},"text":"old"}}]}`), nil
			}
			if request.URL.Query().Get("offset") != "6" {
				t.Fatalf("reply offset=%q, want 6", request.URL.Query().Get("offset"))
			}
			return telegramResponse(http.StatusOK, `{"ok":true,"result":[{"update_id":6,"message":{"chat":{"id":123},"text":"Cookie: acf_uid=1; dy_did=2"}}]}`), nil
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			sent = append(sent, payload["text"])
			return telegramResponse(http.StatusOK, `{"ok":true}`), nil
		default:
			t.Fatalf("unexpected Telegram request: %s", request.URL)
			return nil, nil
		}
	}), time.Second, 60)
	a := New(testConfig(), providerFunc(func(_ context.Context, values map[string]string) ([]model.Room, error) {
		if !reflect.DeepEqual(values, expected) {
			t.Fatalf("validated=%#v, want %#v", values, expected)
		}
		return []model.Room{room("dy_1", true)}, nil
	}), store, notifier, &bytes.Buffer{})

	values, rooms, err := a.recoverCookies(context.Background(), "expired <cookie>")
	if err != nil || !reflect.DeepEqual(values, expected) || len(rooms) != 1 || updatesCalls != 2 {
		t.Fatalf("values=%#v rooms=%#v updates=%d err=%v", values, rooms, updatesCalls, err)
	}
	if got := store.Load(); !reflect.DeepEqual(got, expected) {
		t.Fatalf("saved=%#v, want %#v", got, expected)
	}
	if len(sent) != 2 || !strings.Contains(sent[0], "expired &lt;cookie&gt;") {
		t.Fatalf("sent=%#v", sent)
	}
}

func TestRecoverCookiesRetriesInvalidAndUnverifiableReplies(t *testing.T) {
	store := &memoryStore{}
	notifier := &fakeNotifier{nextOffset: 6}
	messages := []string{"not cookies", "bad=1", "unverified=2", "good=3"}
	waitCalls := 0
	notifier.wait = func(_ context.Context, offset int64) (string, int64, error) {
		if offset != int64(6+waitCalls) {
			t.Fatalf("offset=%d, want %d", offset, 6+waitCalls)
		}
		message := messages[waitCalls]
		waitCalls++
		return message, offset + 1, nil
	}
	cfg := testConfig()
	cfg.ValidationRetries = 1
	calls := 0
	a := New(cfg, providerFunc(func(_ context.Context, values map[string]string) ([]model.Room, error) {
		calls++
		switch {
		case values["bad"] == "1":
			return nil, &douyu.NotLoginError{Message: "rejected"}
		case values["unverified"] == "2":
			return nil, &douyu.APIError{Message: "down <temporarily>"}
		case values["good"] == "3":
			return []model.Room{room("dy_1", true)}, nil
		default:
			t.Fatalf("unexpected cookies=%#v", values)
			return nil, nil
		}
	}), store, notifier, &bytes.Buffer{})

	values, rooms, err := a.recoverCookies(context.Background(), "expired")
	want := map[string]string{"good": "3"}
	if err != nil || !reflect.DeepEqual(values, want) || len(rooms) != 1 || !reflect.DeepEqual(store.values, want) || calls != 3 || waitCalls != 4 {
		t.Fatalf("values=%#v rooms=%#v saved=%#v calls=%d waits=%d err=%v", values, rooms, store.values, calls, waitCalls, err)
	}
	joined := strings.Join(notifier.sent, "\n")
	for _, want := range []string{"could not parse", "did not work", "down &lt;temporarily&gt;", "verified successfully"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sent=%#v missing %q", notifier.sent, want)
		}
	}
}

func TestRecoverCookiesReportsPollingConflicts(t *testing.T) {
	conflict := &telegram.PollingConflict{Message: "other <bot>"}
	for _, tt := range []struct {
		name     string
		notifier *fakeNotifier
		wantSent int
		wantText string
	}{
		{"initial offset", &fakeNotifier{nextErr: conflict}, 1, "I could send notifications"},
		{"cookie reply", &fakeNotifier{wait: func(context.Context, int64) (string, int64, error) { return "", 8, conflict }}, 2, "I cannot receive your cookie reply"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := New(testConfig(), nil, &memoryStore{}, tt.notifier, &bytes.Buffer{}).recoverCookies(context.Background(), "expired")
			var got *telegram.PollingConflict
			if !errors.As(err, &got) || len(tt.notifier.sent) != tt.wantSent {
				t.Fatalf("err=%v sent=%#v", err, tt.notifier.sent)
			}
			last := tt.notifier.sent[len(tt.notifier.sent)-1]
			if !strings.Contains(last, tt.wantText) || !strings.Contains(last, "other &lt;bot&gt;") {
				t.Fatalf("last conflict message=%q", last)
			}
		})
	}
}

func TestRunPollBehavior(t *testing.T) {
	calls := 0
	provider := providerFunc(func(context.Context, map[string]string) ([]model.Room, error) {
		calls++
		if calls == 1 {
			return []model.Room{room("dy_1", true)}, nil
		}
		return []model.Room{room("dy_1", false)}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	notifier := &fakeNotifier{}
	notifier.ping = func(_ context.Context, offset int64, _ int) (int64, error) { cancel(); return offset + 1, nil }
	a := New(testConfig(), provider, &memoryStore{values: map[string]string{"a": "b"}}, notifier, &bytes.Buffer{})
	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) || calls != 2 || len(notifier.previousArgs) != 2 || len(notifier.roomArgs) != 2 || len(notifier.previousArgs[1]) != 1 {
		t.Fatalf("err=%v calls=%d previous=%#v rooms=%#v", err, calls, notifier.previousArgs, notifier.roomArgs)
	}
	if !notifier.roomArgs[0][0].IsLive || notifier.roomArgs[1][0].IsLive {
		t.Fatalf("notification snapshots=%#v", notifier.roomArgs)
	}
}

func TestRunContinuesAfterDouyuAPIFailures(t *testing.T) {
	cfg := testConfig()
	cfg.ValidationRetries = 1
	calls := 0
	provider := providerFunc(func(context.Context, map[string]string) ([]model.Room, error) {
		calls++
		return nil, &douyu.APIError{Message: "Request failed: HTTP status 502 Bad Gateway"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier := &fakeNotifier{}
	var pingTimeout int
	notifier.ping = func(_ context.Context, _ int64, timeout int) (int64, error) {
		pingTimeout = timeout
		cancel()
		return 0, nil
	}
	var output bytes.Buffer
	err := New(cfg, provider, &memoryStore{values: map[string]string{"a": "b"}}, notifier, &output).Run(ctx)
	if !errors.Is(err, context.Canceled) || calls != 2 || pingTimeout < 1 || pingTimeout > 30 {
		t.Fatalf("err=%v calls=%d timeout=%d", err, calls, pingTimeout)
	}
	for _, want := range []string{"Initial validation failed: Request failed: HTTP status 502 Bad Gateway", "Starting monitor anyway", "API Error: Request failed: HTTP status 502 Bad Gateway", "Retrying in 30 seconds"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output=%q missing %q", output.String(), want)
		}
	}
}

func TestRunRequiresTelegramConfig(t *testing.T) {
	cfg := testConfig()
	cfg.BotToken = ""
	err := New(cfg, nil, nil, nil, &bytes.Buffer{}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("err=%v", err)
	}
}
