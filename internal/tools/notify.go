package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
)

// NotifyUserTool allows the agent to push an asynchronous message to the user
// on whatever channel the conversation originated from.  It uses the existing
// AnnounceToSession infrastructure.
type NotifyUserTool struct {
	Announcers []agentapi.Announcer
}

type notifyInput struct {
	Message string `json:"message"`
}

func (t *NotifyUserTool) Name() string { return "notify_user" }

func (t *NotifyUserTool) Description() string {
	return "Send a message to the user right now on their current channel. " +
		"Use this when you need to proactively inform the user of something — " +
		"for example, reporting the result of an action that just completed, " +
		"or alerting them to something important you discovered. " +
		"The message is delivered immediately and appears as a new message in their chat."
}

func (t *NotifyUserTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"message": {
				"type": "string",
				"description": "The text message to send to the user"
			}
		},
		"required": ["message"]
	}`)
}

func (t *NotifyUserTool) Run(ctx context.Context, argsJSON string) string {
	var input notifyInput
	if err := json.Unmarshal([]byte(argsJSON), &input); err != nil {
		return fmt.Sprintf("error: invalid input: %v", err)
	}
	if input.Message == "" {
		return "error: message is required"
	}

	sessionKey, _ := ctx.Value(SessionKeyContextKey{}).(string)
	if sessionKey == "" {
		return "no session context — cannot deliver notification"
	}

	delivered := 0
	for _, a := range t.Announcers {
		a.AnnounceToSession(sessionKey, input.Message)
		delivered++
	}
	if delivered == 0 {
		return "no delivery channel available for this session"
	}
	return fmt.Sprintf("message delivered to user via %d channel(s)", delivered)
}
