package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// authURL is the Google OAuth2 consent endpoint. Var so tests can override.
var authURL = "https://accounts.google.com/o/oauth2/v2/auth"

// DefaultConfigPath is where `stdd drive auth` persists credentials:
// ~/.config/stdd/drive.json.
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "stdd", "drive.json"), nil
}

// LoadConfig reads a persisted Config. A missing file returns an error
// satisfying os.IsNotExist.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("drive: parse %s: %w", path, err)
	}
	return cfg, nil
}

// SaveConfig persists cfg with owner-only permissions.
func SaveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ParseInstalledCredentials extracts the client id and secret from a
// "Desktop app" OAuth client JSON file downloaded from the Google Cloud
// console.
func ParseInstalledCredentials(path string) (clientID, clientSecret string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var f struct {
		Installed struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return "", "", fmt.Errorf("drive: parse %s: %w", path, err)
	}
	if f.Installed.ClientID == "" {
		return "", "", fmt.Errorf("drive: %s has no installed.client_id (download a Desktop app OAuth client)", path)
	}
	return f.Installed.ClientID, f.Installed.ClientSecret, nil
}

// Authorize runs the OAuth2 installed-app flow: it prints a consent URL to
// out, catches the redirect on a local listener, exchanges the code, and
// returns a Config carrying the refresh token. Blocks until the browser
// round-trip completes or ctx ends.
func Authorize(ctx context.Context, clientID, clientSecret string, out io.Writer) (Config, error) {
	if clientID == "" || clientSecret == "" {
		return Config{}, errors.New("drive: client id and secret are required")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Config{}, fmt.Errorf("drive: local listener: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://%s/", ln.Addr().String())

	consent := authURL + "?" + url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {Scope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
	}.Encode()
	fmt.Fprintf(out, "Open this URL in your browser to authorize Drive access:\n\n  %s\n\nWaiting for authorization...\n", consent)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			errCh <- fmt.Errorf("drive: authorization denied: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "stdd: Drive authorized. You can close this window.")
		codeCh <- code
	})}
	go srv.Serve(ln)
	defer srv.Close()

	var code string
	select {
	case <-ctx.Done():
		return Config{}, ctx.Err()
	case err := <-errCh:
		return Config{}, err
	case code = <-codeCh:
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirect},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, defaultTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Config{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Config{}, fmt.Errorf("drive: exchange code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Config{}, fmt.Errorf("drive: exchange code: %s: %s", resp.Status, body)
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return Config{}, fmt.Errorf("drive: exchange code: %w", err)
	}
	if tok.RefreshToken == "" {
		return Config{}, errors.New("drive: no refresh token returned (revoke the app's access and retry)")
	}
	return Config{ClientID: clientID, ClientSecret: clientSecret, RefreshToken: tok.RefreshToken}, nil
}
