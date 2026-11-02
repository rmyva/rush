package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Health reports service health.
func (c *Client) Health(ctx context.Context) error {
	var response struct {
		Status string `json:"status"`
	}
	return c.getJSON(ctx, "/health", nil, &response)
}

// Version returns the server's version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	var response struct {
		Version string `json:"version"`
	}
	if err := c.getJSON(ctx, "/version", nil, &response); err != nil {
		return "", err
	}
	return response.Version, nil
}

// Models lists available models.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var models []Model
	if err := c.getJSON(ctx, "/models", nil, &models); err != nil {
		return nil, err
	}
	if models == nil {
		models = []Model{}
	}
	return models, nil
}

// OpenAPI returns the OpenAPI document bytes, subject to the configured response-size limit.
func (c *Client) OpenAPI(ctx context.Context) ([]byte, error) {
	_, _, data, err := c.do(ctx, http.MethodGet, "/openapi.json", nil, nil, reqOpts{})
	return data, err
}

// ListPermissions lists pending permissions matching filter.
func (c *Client) ListPermissions(ctx context.Context, filter PermissionFilter) ([]Permission, error) {
	var response struct {
		Items []Permission `json:"items"`
	}
	query := make(url.Values)
	if filter.SessionID != "" {
		query.Set("session_id", filter.SessionID)
	}
	if filter.RunID != "" {
		query.Set("run_id", filter.RunID)
	}
	if filter.State != "" {
		query.Set("state", filter.State)
	}
	if err := c.getJSON(ctx, "/permissions", query, &response); err != nil {
		return nil, err
	}
	if response.Items == nil {
		response.Items = []Permission{}
	}
	return response.Items, nil
}

// AnswerPermission allows or denies a permission request.
func (c *Client) AnswerPermission(ctx context.Context, id string, allow bool) error {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	return c.mutateJSON(ctx, http.MethodPost, "/permissions/"+url.PathEscape(id), struct {
		Decision string `json:"decision"`
	}{decision}, "", nil)
}

// Raw sends a request through the configured transport and path validation.
func (c *Client) Raw(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var payload any
	if body != nil {
		payload = json.RawMessage(body)
	}
	status, _, response, err := c.do(ctx, method, path, nil, payload, reqOpts{})
	return response, status, err
}

func (c *Client) getJSON(ctx context.Context, path string, query any, target any) error {
	_, _, data, err := c.do(ctx, http.MethodGet, path, query, nil, reqOpts{})
	if err != nil {
		return err
	}
	if target == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func (c *Client) mutateJSON(ctx context.Context, method, path string, body any, key string, target any) error {
	_, _, data, err := c.do(ctx, method, path, nil, body, reqOpts{IdempotencyKey: key})
	if err != nil {
		return err
	}
	if target == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func paginationQuery(opts ListOptions) url.Values {
	query := make(url.Values)
	if opts.After != "" {
		query.Set("after", opts.After)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	return query
}

func collectPages[T any](ctx context.Context, max int, list func(context.Context, string, int) (*Page[T], error)) ([]T, error) {
	if max < 0 {
		return nil, errors.New("maximum item count must not be negative")
	}
	items := make([]T, 0)
	after := ""
	seen := make(map[string]struct{})
	for {
		limit := 200
		if remaining := max - len(items); remaining < limit {
			limit = remaining + 1
		}
		if limit < 1 {
			limit = 1
		}
		page, err := list(ctx, after, limit)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if len(items) > max {
			return nil, fmt.Errorf("result exceeds maximum of %d items", max)
		}
		if page.NextAfter == nil {
			return items, nil
		}
		if _, duplicate := seen[*page.NextAfter]; duplicate || *page.NextAfter == after {
			return nil, errors.New("pagination cursor repeated")
		}
		seen[*page.NextAfter] = struct{}{}
		after = *page.NextAfter
	}
}
