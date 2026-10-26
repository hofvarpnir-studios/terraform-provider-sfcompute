package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Client is the configured provider: an authenticated SF Compute API client
// plus the provider-level settings resources need.
type Client struct {
	BaseURL   *url.URL
	Token     string
	HTTP      *http.Client
	UserAgent string

	// DefaultWorkspace fills `workspace` when a resource or data source
	// leaves it unset.
	DefaultWorkspace string
	// AllowSpending must be true to create resources whose definition has
	// Spending set.
	AllowSpending bool

	MaxRetries   int
	RetryWaitMin time.Duration
	RetryWaitMax time.Duration
	// DeleteConflictTimeout bounds how long a delete keeps retrying while
	// the API answers 409 Conflict (e.g. an instance still terminating).
	DeleteConflictTimeout time.Duration
}

// NewClient returns a Client with default retry settings.
func NewClient(baseURL, token, userAgent string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid base_url %q: %w", baseURL, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("invalid base_url %q: must be an absolute http(s) URL", baseURL)
	}
	return &Client{
		BaseURL:               u,
		Token:                 token,
		HTTP:                  &http.Client{Timeout: 2 * time.Minute},
		UserAgent:             userAgent,
		MaxRetries:            4,
		RetryWaitMin:          time.Second,
		RetryWaitMax:          30 * time.Second,
		DeleteConflictTimeout: 5 * time.Minute,
	}, nil
}

// APIError is a non-2xx response.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Type    string
	Message string
	Details []string
	Body    string
}

func (e *APIError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Type != "" {
		fmt.Fprintf(&sb, " (%s)", e.Type)
	}
	if e.Message != "" {
		sb.WriteString(": ")
		sb.WriteString(e.Message)
	} else if e.Body != "" {
		sb.WriteString(": ")
		sb.WriteString(e.Body)
	}
	for _, d := range e.Details {
		sb.WriteString("\n  - ")
		sb.WriteString(d)
	}
	return sb.String()
}

// StatusOf returns the HTTP status of an *APIError, or 0.
func StatusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// Request is one API call.
type Request struct {
	Method string
	Path   string // expanded and escaped (see ExpandPath), starting with "/"
	Query  url.Values
	Body   any // JSON-encoded when non-nil
	// IdempotencyHeader, if set, is sent with a random key that stays the
	// same across retries, which makes retrying a POST safe.
	IdempotencyHeader string
}

// Do performs a request and decodes a JSON response (numbers as
// json.Number). A response without a body yields nil.
func (c *Client) Do(ctx context.Context, r Request) (any, error) {
	if c.Token == "" {
		return nil, errors.New("no SF Compute API token configured: set `token` in the provider block or the SFC_TOKEN environment variable")
	}
	full := strings.TrimRight(c.BaseURL.String(), "/") + r.Path
	if len(r.Query) > 0 {
		full += "?" + r.Query.Encode()
	}
	var payload []byte
	if r.Body != nil {
		var err error
		payload, err = json.Marshal(r.Body)
		if err != nil {
			return nil, err
		}
	}
	idemKey := ""
	if r.IdempotencyHeader != "" {
		idemKey = randomKey()
	}
	retryable := r.Method != http.MethodPost || idemKey != ""

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, r.Method, full, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")
		if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idemKey != "" {
			req.Header.Set(r.IdempotencyHeader, idemKey)
		}
		tflog.Debug(ctx, "SF Compute API request", map[string]any{"method": r.Method, "url": full, "attempt": attempt + 1})

		resp, err := c.HTTP.Do(req)
		var wait time.Duration
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("%s %s: %w", r.Method, r.Path, err)
			if !retryable {
				return nil, lastErr
			}
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			tflog.Debug(ctx, "SF Compute API response", map[string]any{"method": r.Method, "url": full, "status": resp.StatusCode})
			if readErr != nil {
				lastErr = fmt.Errorf("%s %s: reading response: %w", r.Method, r.Path, readErr)
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return decodeJSON(body, r)
			} else {
				lastErr = newAPIError(r, resp.StatusCode, body)
				if !retryableStatus(resp.StatusCode) || !retryable && resp.StatusCode != http.StatusTooManyRequests {
					return nil, lastErr
				}
				wait = retryAfter(resp.Header.Get("Retry-After"))
			}
		}
		if attempt >= c.MaxRetries {
			return nil, lastErr
		}
		if wait == 0 {
			wait = c.backoff(attempt)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func decodeJSON(body []byte, r Request) (any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s %s: decoding response: %w", r.Method, r.Path, err)
	}
	return v, nil
}

func newAPIError(r Request, status int, body []byte) *APIError {
	e := &APIError{Method: r.Method, Path: r.Path, Status: status}
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Details []struct {
				Code    string  `json:"code"`
				Field   *string `json:"field"`
				Message string  `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && (parsed.Error.Message != "" || parsed.Error.Type != "") {
		e.Type = parsed.Error.Type
		e.Message = parsed.Error.Message
		for _, d := range parsed.Error.Details {
			s := d.Message
			if d.Field != nil && *d.Field != "" {
				s = *d.Field + ": " + s
			}
			if d.Code != "" {
				s += " [" + d.Code + "]"
			}
			e.Details = append(e.Details, s)
		}
		return e
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	e.Body = s
	return e
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		if d > 2*time.Minute {
			d = 2 * time.Minute
		}
		return d
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 && d < 2*time.Minute {
			return d
		}
	}
	return 0
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.RetryWaitMin << attempt
	if d <= 0 || d > c.RetryWaitMax {
		d = c.RetryWaitMax
	}
	return d
}

func randomKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ExpandPath substitutes {name} placeholders with values, escaping each
// value as one path segment. ":" is kept as is: SF Compute resource paths
// such as sfc:pool:acme:prod:train are documented as URL-safe.
func ExpandPath(tmpl string, values map[string]string) (string, error) {
	var sb strings.Builder
	rest := tmpl
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			sb.WriteString(rest)
			return sb.String(), nil
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return "", fmt.Errorf("malformed path template %q", tmpl)
		}
		name := rest[i+1 : i+j]
		v, ok := values[name]
		if !ok || v == "" {
			return "", fmt.Errorf("no value for path parameter {%s} in %s", name, tmpl)
		}
		sb.WriteString(rest[:i])
		sb.WriteString(strings.ReplaceAll(url.PathEscape(v), "%3A", ":"))
		rest = rest[i+j+1:]
	}
}
