// Package toast publishes user-facing notification events on the websocket
// hub. Any backend code with a hub reference can surface feedback without
// threading UI concerns through domain types — poller failures, scorer
// batch warnings, delegation completions, etc.
//
// Calls are safe with nil hub (no-op) so package-level toast-fires don't
// crash in test paths that skip WS setup.
package toast

import (
	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Level is the severity tier. The frontend picks the visual treatment and
// auto-hide duration off this value: success/info ~3s, warning ~6s, error
// sticky until dismissed.
type Level string

const (
	LevelInfo    Level = "info"
	LevelSuccess Level = "success"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
)

// Payload is the shape the frontend expects inside the WS event's Data.
// Title is optional (short bold header above body); ID enables dedup on
// the client side if the same underlying condition fires twice.
type Payload struct {
	ID    string `json:"id"`
	Level Level  `json:"level"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

// Broadcaster is the minimal surface Fire needs from a hub. *websocket.Hub
// satisfies it; tests can pass a fake.
type Broadcaster interface {
	Broadcast(websocket.Event)
}

// Fire publishes a toast with the given level/title/body. No-op when hub
// is nil or body is empty — empty-body toasts would render as confusing
// blank cards, so we drop them silently.
func Fire(hub Broadcaster, level Level, title, body string) {
	if hub == nil || body == "" {
		return
	}
	hub.Broadcast(websocket.Event{
		Type: "toast",
		Data: Payload{
			ID:    uuid.New().String(),
			Level: level,
			Title: title,
			Body:  body,
		},
	})
}

// Convenience helpers — the common case has no title.

func Info(hub Broadcaster, body string)    { Fire(hub, LevelInfo, "", body) }
func Success(hub Broadcaster, body string) { Fire(hub, LevelSuccess, "", body) }
func Warning(hub Broadcaster, body string) { Fire(hub, LevelWarning, "", body) }
func Error(hub Broadcaster, body string)   { Fire(hub, LevelError, "", body) }

// Titled variants for when you want a short category label + a longer body.

func InfoTitled(hub Broadcaster, title, body string) {
	Fire(hub, LevelInfo, title, body)
}

func SuccessTitled(hub Broadcaster, title, body string) {
	Fire(hub, LevelSuccess, title, body)
}

func WarningTitled(hub Broadcaster, title, body string) {
	Fire(hub, LevelWarning, title, body)
}

func ErrorTitled(hub Broadcaster, title, body string) {
	Fire(hub, LevelError, title, body)
}
