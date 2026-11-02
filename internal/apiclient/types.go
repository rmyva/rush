package apiclient

import (
	"encoding/json"
	"time"
)

// Session is a safe summary of an API session.
type Session struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	MessageCount int64  `json:"message_count"`
}

// Message is a message in a session.
type Message struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Role      string        `json:"role"`
	Parts     []MessagePart `json:"parts"`
	CreatedAt string        `json:"created_at"`
}

// MessagePart preserves unrecognized part types in Raw.
type MessagePart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Input      string          `json:"input,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Content    string          `json:"content,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps unknown part types as their original JSON object.
func (p *MessagePart) UnmarshalJSON(data []byte) error {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	p.Type = header.Type
	switch p.Type {
	case "text", "reasoning", "tool_call", "tool_result":
		type known MessagePart
		return json.Unmarshal(data, (*known)(p))
	default:
		p.Raw = append(p.Raw[:0], data...)
		return nil
	}
}

// MarshalJSON returns unknown parts unchanged, and known parts with wire fields.
func (p MessagePart) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case "text", "reasoning", "tool_call", "tool_result":
		type known MessagePart
		return json.Marshal(known(p))
	default:
		if len(p.Raw) == 0 {
			return json.Marshal(struct {
				Type string `json:"type"`
			}{Type: p.Type})
		}
		return append([]byte(nil), p.Raw...), nil
	}
}

// Model describes a model available to the API.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Role     string `json:"role,omitempty"`
}

// Run describes the current state of an asynchronous run.
type Run struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"session_id"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      *RunError  `json:"error,omitempty"`
	MessageID  string     `json:"message_id,omitempty"`
}

// RunError is the server-reported failure for a run.
type RunError struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
	Run     *Run   `json:"-"`
}

func (e *RunError) Error() string {
	if e == nil {
		return "run failed"
	}
	if e.Summary != "" {
		return e.Summary
	}
	if e.Code != "" {
		return e.Code
	}
	return "run failed"
}

// RunAccepted describes an accepted run submission.
type RunAccepted struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
}

// Permission represents a pending tool permission request.
type Permission struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	RunID       string    `json:"run_id"`
	Tool        string    `json:"tool"`
	Action      string    `json:"action"`
	Description string    `json:"description"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
}

// Page is a cursor-paginated response.
type Page[T any] struct {
	Items     []T     `json:"items"`
	NextAfter *string `json:"next_after"`
}

// RunRequest is the body used to create a run.
type RunRequest struct {
	Prompt string `json:"prompt"`
	Role   string `json:"role,omitempty"`
	Model  string `json:"model,omitempty"`
}

// ListOptions contains common cursor pagination parameters.
type ListOptions struct {
	After string
	Limit int
}

// PermissionFilter filters permission requests by session or run.
type PermissionFilter struct {
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	State     string `json:"state,omitempty"`
}
