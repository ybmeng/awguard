// Package drive is a minimal, dependency-free Google Drive v3 client
// covering exactly what std services need: OAuth2 refresh-token auth,
// folder find-or-create, idempotent file upload, lookup, and download.
package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultTokenURL   = "https://oauth2.googleapis.com/token"
	defaultAPIBase    = "https://www.googleapis.com/drive/v3"
	defaultUploadBase = "https://www.googleapis.com/upload/drive/v3"

	// Scope limits access to files created by this app.
	Scope = "https://www.googleapis.com/auth/drive.file"

	folderMIME = "application/vnd.google-apps.folder"
)

// Config holds the OAuth2 credentials for a Client. The JSON fields are what
// gets persisted by SaveConfig after `stdd drive auth`.
type Config struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`

	// Endpoint overrides, used by tests. Empty means the real Google APIs.
	TokenURL   string       `json:"-"`
	APIBase    string       `json:"-"`
	UploadBase string       `json:"-"`
	HTTPClient *http.Client `json:"-"`
}

// Client is a Google Drive v3 API client. It refreshes its access token
// automatically and is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client

	mu          sync.Mutex
	accessToken string
	expiry      time.Time
}

// NewClient validates cfg and returns a ready Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RefreshToken == "" {
		return nil, errors.New("drive: client_id, client_secret and refresh_token are required (run: stdd drive auth)")
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = defaultTokenURL
	}
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPIBase
	}
	if cfg.UploadBase == "" {
		cfg.UploadBase = defaultUploadBase
	}
	c := &Client{cfg: cfg, http: cfg.HTTPClient}
	if c.http == nil {
		c.http = &http.Client{Timeout: 5 * time.Minute}
	}
	return c, nil
}

// token returns a valid access token, refreshing it when needed.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Until(c.expiry) > time.Minute {
		return c.accessToken, nil
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"refresh_token": {c.cfg.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("drive: refresh token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("drive: refresh token: %s: %s", resp.Status, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("drive: refresh token: %w", err)
	}
	c.accessToken = tok.AccessToken
	c.expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// do sends an authenticated request and returns the response, converting
// non-2xx statuses into errors.
func (c *Client) do(ctx context.Context, method, rawURL, contentType string, body io.Reader) (*http.Response, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("drive: %s %s: %w", method, rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("drive: %s %s: %s: %s", method, rawURL, resp.Status, msg)
	}
	return resp, nil
}

// doJSON is do plus JSON-decoding the response into out (which may be nil).
func (c *Client) doJSON(ctx context.Context, method, rawURL, contentType string, body io.Reader, out any) error {
	resp, err := c.do(ctx, method, rawURL, contentType, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// escapeQuery escapes a string for inclusion in a Drive q= query literal.
func escapeQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

// findByName returns the id of the first non-trashed item with the given
// name (and parent / mimeType when non-empty), or "" if none exists.
func (c *Client) findByName(ctx context.Context, name, parentID, mimeType string) (string, error) {
	q := fmt.Sprintf("name = '%s' and trashed = false", escapeQuery(name))
	if parentID != "" {
		q += fmt.Sprintf(" and '%s' in parents", escapeQuery(parentID))
	}
	if mimeType != "" {
		q += fmt.Sprintf(" and mimeType = '%s'", mimeType)
	}
	u := fmt.Sprintf("%s/files?q=%s&fields=files(id,name)&pageSize=1", c.cfg.APIBase, url.QueryEscape(q))

	var result struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := c.doJSON(ctx, http.MethodGet, u, "", nil, &result); err != nil {
		return "", err
	}
	if len(result.Files) == 0 {
		return "", nil
	}
	return result.Files[0].ID, nil
}

// FindFolder returns the id of the named folder under parentID, or "" if it
// does not exist. Empty parentID searches without a parent constraint.
func (c *Client) FindFolder(ctx context.Context, name, parentID string) (string, error) {
	return c.findByName(ctx, name, parentID, folderMIME)
}

// FindFile returns the id of the named non-folder file under parentID, or ""
// if it does not exist.
func (c *Client) FindFile(ctx context.Context, name, parentID string) (string, error) {
	return c.findByName(ctx, name, parentID, "")
}

// FindOrCreateFolder returns the id of the named folder under parentID,
// creating it when missing.
func (c *Client) FindOrCreateFolder(ctx context.Context, name, parentID string) (string, error) {
	if id, err := c.FindFolder(ctx, name, parentID); err != nil || id != "" {
		return id, err
	}
	meta := map[string]any{"name": name, "mimeType": folderMIME}
	if parentID != "" {
		meta["parents"] = []string{parentID}
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	u := c.cfg.APIBase + "/files?fields=id"
	if err := c.doJSON(ctx, http.MethodPost, u, "application/json", bytes.NewReader(body), &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// Upload puts content into Drive as name under parentID and returns the file
// id. It is idempotent by name: an existing file with the same name in the
// same folder has its content replaced instead of a duplicate being created.
func (c *Client) Upload(ctx context.Context, name, parentID string, content io.Reader) (string, error) {
	existing, err := c.FindFile(ctx, name, parentID)
	if err != nil {
		return "", err
	}
	if existing != "" {
		u := fmt.Sprintf("%s/files/%s?uploadType=media&fields=id", c.cfg.UploadBase, url.PathEscape(existing))
		if err := c.doJSON(ctx, http.MethodPatch, u, "application/octet-stream", content, nil); err != nil {
			return "", err
		}
		return existing, nil
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	metaPart, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return "", err
	}
	meta := map[string]any{"name": name}
	if parentID != "" {
		meta["parents"] = []string{parentID}
	}
	if err := json.NewEncoder(metaPart).Encode(meta); err != nil {
		return "", err
	}
	mediaPart, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(mediaPart, content); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	var created struct {
		ID string `json:"id"`
	}
	u := c.cfg.UploadBase + "/files?uploadType=multipart&fields=id"
	contentType := "multipart/related; boundary=" + w.Boundary()
	if err := c.doJSON(ctx, http.MethodPost, u, contentType, &buf, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// Download streams the content of a Drive file. The caller must close the
// returned reader.
func (c *Client) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/files/%s?alt=media", c.cfg.APIBase, url.PathEscape(fileID))
	resp, err := c.do(ctx, http.MethodGet, u, "", nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
