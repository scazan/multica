// Package asana is the HTTP client and shared types for the Asana task
// tracker integration. Multica mirrors tasks from bound Asana projects into
// issues so agents can own them, and posts agent activity back as stories.
//
// Only the parts that talk to Asana live here. Storage, issue mapping, and
// webhook handling live in the handler and service layers, which consume the
// types below. GitHub and the self-hosted Git providers are deliberately NOT
// modeled on this package: they mirror pull requests onto existing issues,
// whereas Asana is a source of issues.
package asana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the public Asana REST endpoint. Tests point the client at
// an httptest server instead.
const DefaultBaseURL = "https://app.asana.com/api/1.0"

// ErrUnauthorized is returned when Asana rejects the token (HTTP 401/403).
// Callers surface it as a connect-time validation failure distinct from
// transport errors.
var ErrUnauthorized = errors.New("asana: token unauthorized")

// ErrNotFound is returned when Asana reports the resource does not exist or
// the token cannot see it (HTTP 404).
var ErrNotFound = errors.New("asana: not found")

// APIError is any other non-2xx answer from Asana, with the first message
// Asana included so operators can act on it.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("asana: unexpected status %d", e.Status)
	}
	return fmt.Sprintf("asana: status %d: %s", e.Status, e.Message)
}

// Workspace is an Asana workspace (or organization) the token can see.
type Workspace struct {
	GID  string `json:"gid"`
	Name string `json:"name"`
}

// User is the identity behind an access token.
type User struct {
	GID        string      `json:"gid"`
	Name       string      `json:"name"`
	Email      string      `json:"email"`
	Workspaces []Workspace `json:"workspaces"`
}

// Client talks to one Asana account. It is cheap to construct; the handler
// builds one per request from the stored token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithBaseURL overrides the API endpoint (tests).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the transport.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// NewClient returns a client authenticated with a personal access token or an
// OAuth bearer token.
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		token:   token,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Me validates the token and returns the user it belongs to, including the
// workspaces the user can see.
func (c *Client) Me(ctx context.Context) (User, error) {
	var out User
	q := url.Values{"opt_fields": {"name,email,workspaces.name"}}
	if err := c.do(ctx, http.MethodGet, "/users/me", q, nil, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

// envelope is Asana's standard `{"data": ...}` wrapper. Errors come back as
// `{"errors":[{"message":...}]}`.
type envelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// do performs one JSON request. body (if any) is wrapped in the `data`
// envelope Asana expects on writes; out (if any) receives the unwrapped
// `data` member.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(map[string]any{"data": body})
		if err != nil {
			return fmt.Errorf("asana: encode request: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Asana error bodies are small; cap reads so a misbehaving proxy cannot
	// make us buffer an unbounded response.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("asana: read response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		var env envelope
		_ = json.Unmarshal(raw, &env)
		msg := ""
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("asana: decode response: %w", err)
	}
	if len(env.Data) == 0 {
		return fmt.Errorf("asana: response has no data member")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("asana: decode data: %w", err)
	}
	return nil
}
