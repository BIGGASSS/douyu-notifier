package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/BIGGASSS/douyu-notifier/internal/cookies"
	"github.com/BIGGASSS/douyu-notifier/internal/douyu"
	"github.com/BIGGASSS/douyu-notifier/internal/model"
	"github.com/BIGGASSS/douyu-notifier/internal/telegram"
)

type Provider interface {
	Fetch(context.Context, map[string]string) ([]model.Room, error)
}
type CookieStore interface {
	Load() map[string]string
	Save(map[string]string)
}
type Notifier interface {
	Send(context.Context, string) bool
	NextUpdateOffset(context.Context) (int64, error)
	WaitForChatMessage(context.Context, int64) (string, int64, error)
	ProcessPingCommands(context.Context, int64, int) (int64, error)
	UpdateHealth(int)
	ProcessNotifications(context.Context, []model.Room, map[string]struct{}) map[string]struct{}
}

type Config struct {
	BotToken, ChatID                           string
	PollInterval, ValidationRetryDelay         time.Duration
	ValidationRetries, TelegramLongPollTimeout int
}

type App struct {
	config   Config
	provider Provider
	cookies  CookieStore
	notifier Notifier
	out      io.Writer
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

func New(config Config, provider Provider, store CookieStore, notifier Notifier, out io.Writer) *App {
	return &App{config: config, provider: provider, cookies: store, notifier: notifier, out: out, now: time.Now, sleep: sleepContext}
}

func (a *App) printf(format string, args ...any) { fmt.Fprintf(a.out, format, args...) }

func (a *App) validateCookies(ctx context.Context, values map[string]string) ([]model.Room, error) {
	var last *douyu.APIError
	for attempt := 1; attempt <= a.config.ValidationRetries; attempt++ {
		rooms, err := a.provider.Fetch(ctx, values)
		if err == nil {
			return rooms, nil
		}
		var notLoggedIn *douyu.NotLoginError
		if errors.As(err, &notLoggedIn) {
			return nil, err
		}
		var apiError *douyu.APIError
		if !errors.As(err, &apiError) {
			return nil, err
		}
		last = apiError
		if attempt == a.config.ValidationRetries {
			break
		}
		a.printf("Cookie validation hit a temporary API error (%d/%d): %v\n", attempt, a.config.ValidationRetries, apiError)
		if err := a.sleep(ctx, a.config.ValidationRetryDelay); err != nil {
			return nil, err
		}
	}
	if last != nil {
		return nil, last
	}
	return nil, &douyu.APIError{Message: "Cookie validation failed."}
}

func (a *App) recoverCookies(ctx context.Context, reason string) (map[string]string, []model.Room, error) {
	a.printf("Authentication requires a new cookie: %s\n", reason)
	nextOffset, err := a.notifier.NextUpdateOffset(ctx)
	if err != nil {
		a.reportConflict(ctx, err, "I could send notifications, but I cannot receive your reply because this bot token has a Telegram polling conflict.\nReason: ")
		return nil, nil, err
	}
	if a.notifier.Send(ctx, "Douyu cookie expired or is invalid.\nReason: <code>"+telegram.Escape(reason)+"</code>\nReply in this chat with a fresh full cookie string in the format:\n<code>name=value; name2=value2</code>") {
		a.printf("Sent Telegram prompt for a new cookie.\n")
	} else {
		a.printf("Failed to send Telegram prompt; still listening for chat replies.\n")
	}
	for {
		a.printf("Waiting for a new cookie in Telegram...\n")
		message, offset, err := a.notifier.WaitForChatMessage(ctx, nextOffset)
		nextOffset = offset
		if err != nil {
			a.reportConflict(ctx, err, "I cannot receive your cookie reply because this bot token has a Telegram polling conflict.\nReason: ")
			return nil, nil, err
		}
		if message == "" {
			continue
		}
		candidate := cookies.Parse(message)
		if len(candidate) == 0 {
			a.printf("Received a Telegram message that could not be parsed as cookies.\n")
			a.notifier.Send(ctx, "I could not parse that message as a cookie string.\nSend the full value in this format:\n<code>name=value; name2=value2</code>")
			continue
		}
		a.printf("Received %d cookies from Telegram; validating...\n", len(candidate))
		rooms, err := a.validateCookies(ctx, candidate)
		if err != nil {
			var auth *douyu.NotLoginError
			var api *douyu.APIError
			switch {
			case errors.As(err, &auth):
				a.printf("Telegram provided cookie was rejected by Douyu.\n")
				a.notifier.Send(ctx, "That cookie did not work. Make sure it comes from a logged-in Douyu browser session, then send a fresh full cookie string.")
				continue
			case errors.As(err, &api):
				a.printf("Could not validate Telegram cookie due to API error: %v\n", api)
				a.notifier.Send(ctx, "I received your cookie, but Douyu could not be reached to verify it yet: <code>"+telegram.Escape(api.Error())+"</code>\nPlease send the cookie again after the service recovers.")
				continue
			default:
				return nil, nil, err
			}
		}
		a.cookies.Save(candidate)
		a.notifier.Send(ctx, "New Douyu cookie verified successfully. Monitoring resumed.")
		a.printf("Telegram cookie accepted and saved.\n")
		return candidate, rooms, nil
	}
}

func (a *App) reportConflict(ctx context.Context, err error, prefix string) {
	var conflict *telegram.PollingConflict
	if errors.As(err, &conflict) {
		a.printf("Telegram polling conflict: %v\n", conflict)
		a.notifier.Send(ctx, prefix+"<code>"+telegram.Escape(conflict.Error())+"</code>")
	}
}

func (a *App) waitWithPingChecks(ctx context.Context, delay time.Duration, offset int64) (int64, error) {
	deadline := a.now().Add(delay)
	current := offset
	for {
		if err := ctx.Err(); err != nil {
			return current, err
		}
		remaining := deadline.Sub(a.now())
		if remaining <= 0 {
			return current, nil
		}
		timeout := int(remaining / time.Second)
		if timeout < 1 {
			timeout = 1
		}
		if timeout > a.config.TelegramLongPollTimeout {
			timeout = a.config.TelegramLongPollTimeout
		}
		next, err := a.notifier.ProcessPingCommands(ctx, current, timeout)
		if err != nil {
			return current, err
		}
		current = next
	}
}

func (a *App) Run(ctx context.Context) error {
	a.printf("Douyu Live Status Notifier\n==========================\n")
	if a.config.BotToken == "" || a.config.ChatID == "" {
		return errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID env vars required.")
	}
	values := a.cookies.Load()
	var initial []model.Room
	if len(values) == 0 {
		a.printf("No local cookies available. Requesting one through Telegram.\n")
		var err error
		values, initial, err = a.recoverCookies(ctx, "No local cookie found.")
		if err != nil {
			return err
		}
	} else {
		rooms, err := a.validateCookies(ctx, values)
		if err == nil {
			initial = rooms
		} else {
			var auth *douyu.NotLoginError
			var api *douyu.APIError
			switch {
			case errors.As(err, &auth):
				values, initial, err = a.recoverCookies(ctx, auth.Error())
				if err != nil {
					return err
				}
			case errors.As(err, &api):
				a.printf("Initial validation failed: %v\nStarting monitor anyway and retrying in the polling loop.\n", api)
				initial = []model.Room{}
			default:
				return err
			}
		}
	}
	a.printf("Found %d cookies\nPolling every %d seconds\nPress Ctrl+C to stop\n\n", len(values), int(a.config.PollInterval/time.Second))
	previous := a.notifier.ProcessNotifications(ctx, initial, nil)
	var pingOffset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rooms, err := a.provider.Fetch(ctx, values)
		if err == nil {
			live := countLive(rooms)
			a.printf("[%s] Checked: %d/%d live\n", a.now().Format("15:04:05"), live, len(rooms))
			a.notifier.UpdateHealth(live)
			previous = a.notifier.ProcessNotifications(ctx, rooms, previous)
			pingOffset, err = a.waitWithPingChecks(ctx, a.config.PollInterval, pingOffset)
			if err == nil {
				continue
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var auth *douyu.NotLoginError
		if errors.As(err, &auth) {
			a.printf("\nAuthentication Error: %v\n", auth)
			var rooms []model.Room
			values, rooms, err = a.recoverCookies(ctx, auth.Error())
			if err != nil {
				return err
			}
			live := countLive(rooms)
			a.printf("[%s] Recovered with %d/%d live\n", a.now().Format("15:04:05"), live, len(rooms))
			a.notifier.UpdateHealth(live)
			previous = a.notifier.ProcessNotifications(ctx, rooms, previous)
			pingOffset, err = a.waitWithPingChecks(ctx, a.config.PollInterval, pingOffset)
			if err != nil {
				return err
			}
			continue
		}
		var api *douyu.APIError
		if errors.As(err, &api) {
			a.printf("\nAPI Error: %v\nRetrying in 30 seconds...\n", api)
		} else {
			a.printf("\nUnexpected error: %v\nRetrying in 30 seconds...\n", err)
		}
		pingOffset, err = a.waitWithPingChecks(ctx, 30*time.Second, pingOffset)
		if err != nil {
			return err
		}
	}
}

func countLive(rooms []model.Room) int {
	count := 0
	for _, room := range rooms {
		if room.IsLive {
			count++
		}
	}
	return count
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
