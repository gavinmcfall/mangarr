package kavita

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client authenticates against Kavita's plugin API and triggers library scans.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

// New returns a Client targeting the given Kavita base URL with the given API key.
// base and apiKey are injected so tests can supply an httptest server.
func New(base, apiKey string) *Client {
	return &Client{
		base:   base,
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// authenticate exchanges the API key for a short-lived JWT.
// POST /api/plugin/authenticate?apiKey=...&pluginName=mangarr
func (c *Client) authenticate() (string, error) {
	u := fmt.Sprintf("%s/api/plugin/authenticate?apiKey=%s&pluginName=mangarr",
		c.base, url.QueryEscape(c.apiKey))
	resp, err := c.http.Post(u, "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("kavita auth request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) // drain for keep-alive reuse
		return "", fmt.Errorf("kavita auth status %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("kavita auth decode: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("kavita auth: empty token in response")
	}
	return out.Token, nil
}

// Ping verifies that the Kavita credentials are valid by calling authenticate()
// and discarding the token. It is used by the health check to confirm the
// base URL and API key are correct without triggering any library operations.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.authenticate()
	return err
}

// ScanLibrary triggers a full library scan for the given library ID.
// It authenticates first, then POST /api/library/scan with the bearer token.
// libraryID is int64 to match model.Settings.KavitaLibIDs []int64.
func (c *Client) ScanLibrary(libraryID int64) error {
	token, err := c.authenticate()
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]int64{"libraryId": libraryID})
	if err != nil {
		return fmt.Errorf("kavita scan marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/library/scan", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("kavita scan request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kavita scan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body) // drain for keep-alive reuse
		return fmt.Errorf("kavita scan status %d for library %d", resp.StatusCode, libraryID)
	}
	return nil
}
