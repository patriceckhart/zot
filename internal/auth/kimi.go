package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// KimiDeviceAuthorization is the OAuth 2 device-code response used by
// the official Kimi Code CLI.
type KimiDeviceAuthorization struct {
	UserCode                string `json:"user_code"`
	DeviceCode              string `json:"device_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// RequestKimiDeviceAuthorization starts Kimi Code's device-code login.
func RequestKimiDeviceAuthorization(ctx context.Context) (KimiDeviceAuthorization, error) {
	form := bytes.NewBufferString("client_id=" + KimiOAuth.ClientID)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://auth.kimi.com/api/oauth/device_authorization", form)
	if err != nil {
		return KimiDeviceAuthorization{}, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "zot")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return KimiDeviceAuthorization{}, fmt.Errorf("kimi device authorization: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return KimiDeviceAuthorization{}, fmt.Errorf("kimi device authorization http %d: %s", resp.StatusCode, string(body))
	}
	var out KimiDeviceAuthorization
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("parse kimi device authorization: %w", err)
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return out, nil
}

// PollKimiDeviceToken polls until the browser/device-code login completes.
func PollKimiDeviceToken(ctx context.Context, auth KimiDeviceAuthorization) (*OAuthToken, error) {
	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		tok, retry, err := pollKimiDeviceTokenOnce(ctx, auth.DeviceCode)
		if err != nil {
			return nil, err
		}
		if tok != nil {
			return tok, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
}

func pollKimiDeviceTokenOnce(ctx context.Context, deviceCode string) (*OAuthToken, time.Duration, error) {
	form := bytes.NewBufferString("client_id=" + KimiOAuth.ClientID + "&device_code=" + deviceCode + "&grant_type=urn:ietf:params:oauth:grant-type:device_code")
	req, err := http.NewRequestWithContext(ctx, "POST", KimiOAuth.TokenURL, form)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "zot")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("kimi token poll: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var raw struct {
		AccessToken      string  `json:"access_token"`
		RefreshToken     string  `json:"refresh_token"`
		TokenType        string  `json:"token_type"`
		ExpiresIn        float64 `json:"expires_in"`
		Scope            string  `json:"scope"`
		Error            string  `json:"error"`
		ErrorDescription string  `json:"error_description"`
	}
	_ = json.Unmarshal(body, &raw)
	if resp.StatusCode == http.StatusOK && raw.AccessToken != "" {
		return &OAuthToken{
			AccessToken:  raw.AccessToken,
			RefreshToken: raw.RefreshToken,
			TokenType:    raw.TokenType,
			Scope:        raw.Scope,
			ClientID:     KimiOAuth.ClientID,
			Expiry:       time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
		}, 0, nil
	}
	if raw.Error == "authorization_pending" || raw.Error == "slow_down" || resp.StatusCode == http.StatusBadRequest {
		return nil, 5 * time.Second, nil
	}
	if raw.Error != "" {
		return nil, 0, fmt.Errorf("kimi token poll: %s: %s", raw.Error, raw.ErrorDescription)
	}
	return nil, 0, fmt.Errorf("kimi token poll http %d: %s", resp.StatusCode, string(body))
}
