package apiclient

import (
	"context"
	"net/http"
	"net/url"
)

// SessionsOptions selects a page of sessions.
type SessionsOptions struct {
	After string
	Limit int
}

// ListSessions returns one cursor page of sessions.
func (c *Client) ListSessions(ctx context.Context, opts SessionsOptions) (*Page[Session], error) {
	var page Page[Session]
	if err := c.getJSON(ctx, "/sessions", paginationQuery(ListOptions(opts)), &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// AllSessions reads all sessions, stopping if max items would be exceeded.
func (c *Client) AllSessions(ctx context.Context, max int) ([]Session, error) {
	return collectPages(ctx, max, func(ctx context.Context, after string, limit int) (*Page[Session], error) {
		return c.ListSessions(ctx, SessionsOptions{After: after, Limit: limit})
	})
}

// GetSession returns one session by ID.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var item Session
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id), nil, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// CreateSession creates a session with the supplied title.
func (c *Client) CreateSession(ctx context.Context, title string) (*Session, error) {
	var item Session
	if err := c.mutateJSON(ctx, http.MethodPost, "/sessions", map[string]string{"title": title}, "", &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// ForkSession forks a session with the supplied title.
func (c *Client) ForkSession(ctx context.Context, id, title string) (*Session, error) {
	var item Session
	if err := c.mutateJSON(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/fork", map[string]string{"title": title}, "", &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// DeleteSession deletes a session.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	_, _, _, err := c.do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, nil, reqOpts{})
	return err
}

// ListMessages returns one cursor page of messages.
func (c *Client) ListMessages(ctx context.Context, sessionID, after string, limit int) (*Page[Message], error) {
	var page Page[Message]
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(sessionID)+"/messages", paginationQuery(ListOptions{After: after, Limit: limit}), &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// AllMessages reads all messages for a session, stopping if max items would be exceeded.
func (c *Client) AllMessages(ctx context.Context, sessionID string, max int) ([]Message, error) {
	return collectPages(ctx, max, func(ctx context.Context, after string, limit int) (*Page[Message], error) {
		return c.ListMessages(ctx, sessionID, after, limit)
	})
}
