package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Client talks to a running artifacts service over its unix socket. It lets
// other processes (the stdd CLI, other tools) route inserts through the one
// service that owns the store, instead of racing it with direct access.
type Client struct {
	http *http.Client
}

// Dial connects to the service serving root and confirms it is alive with a
// health check. It fails fast when no service is running.
func Dial(ctx context.Context, root string) (*Client, error) {
	sock := SocketPath(root)
	c := &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", sock)
			},
		},
	}}

	healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var health struct {
		OK bool `json:"ok"`
	}
	if err := c.getJSON(healthCtx, "/v1/health", &health); err != nil || !health.OK {
		c.Close()
		return nil, fmt.Errorf("artifacts: no running service for %s: %w", root, err)
	}
	return c, nil
}

// Close releases the client's idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// apiError is the JSON error envelope the service returns.
type apiError struct {
	Error string `json:"error"`
}

func decodeErr(resp *http.Response) error {
	var e apiError
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e); err == nil && e.Error != "" {
		return fmt.Errorf("artifacts service: %s", e.Error)
	}
	return fmt.Errorf("artifacts service: %s", resp.Status)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://artifacts"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeErr(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Insert asks the running service to insert the given files. Paths must be
// absolute; the machine runs in the service, and the id comes back only on
// COMPLETE.
func (c *Client) Insert(ctx context.Context, paths ...string) (ID, error) {
	body, err := json.Marshal(map[string][]string{"paths": paths})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://artifacts/v1/insert", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, decodeErr(resp)
	}
	var result struct {
		ID ID `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	return result.ID, nil
}

// List returns the status of every managed dir, as Store.List does.
func (c *Client) List(ctx context.Context) ([]Status, error) {
	var statuses []Status
	if err := c.getJSON(ctx, "/v1/list", &statuses); err != nil {
		return nil, err
	}
	return statuses, nil
}

// Status returns the state of one managed dir.
func (c *Client) Status(ctx context.Context, id ID) (Status, error) {
	var s Status
	if err := c.getJSON(ctx, "/v1/status/"+id.String(), &s); err != nil {
		return Status{}, err
	}
	return s, nil
}

// Open streams one file of a COMPLETE managed dir through the service
// (local storage or Drive fallback — the service decides).
func (c *Client) Open(ctx context.Context, id ID, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://artifacts/v1/open/"+id.String()+"/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeErr(resp)
	}
	return resp.Body, nil
}
