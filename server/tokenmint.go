package main

// Token minting for the Pi publisher.
//
// The Pi can't mint its own IAP tokens: org policy keeps the service
// private, the Pi has no metadata server, and service-account keys may
// be disallowed. But this server runs inside GCP 24/7, so it mints on
// the Pi's behalf: the same iamcredentials signJwt flow as
// scripts/mint-iap-token.sh, authenticated with this service's own
// runtime service account, delivered to the connected publisher over
// the signaling WebSocket. The publisher persists the token and uses
// it on its next reconnect, so once the Pi has connected a first time
// it stays authenticated indefinitely with no machine outside GCP
// involved.
//
// Enabled by setting TOKEN_MINT_SA and TOKEN_MINT_AUDIENCE (see
// NewTokenMinterFromEnv). The runtime service account needs
// roles/iam.serviceAccountTokenCreator on TOKEN_MINT_SA.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

type TokenMinter struct {
	SA       string        // service account to sign as (must have IAP access)
	Audience string        // IAP audience: https://<service-url>/*
	Interval time.Duration // how often to deliver a fresh token
}

// tokenLifetime is the JWT expiry requested from signJwt. IAP caps
// self-signed JWTs at 1 hour.
const tokenLifetime = time.Hour

// mintRetryDelay is how soon to retry after a failed mint or a mint
// whose delivery failed for a transient reason.
const mintRetryDelay = time.Minute

// NewTokenMinterFromEnv returns a minter configured from TOKEN_MINT_SA,
// TOKEN_MINT_AUDIENCE, and TOKEN_MINT_INTERVAL (Go duration, default
// 30m), or nil when TOKEN_MINT_SA is unset (local dev, no IAP).
func NewTokenMinterFromEnv() *TokenMinter {
	sa := os.Getenv("TOKEN_MINT_SA")
	if sa == "" {
		return nil
	}
	aud := os.Getenv("TOKEN_MINT_AUDIENCE")
	if aud == "" {
		log.Printf("warning: TOKEN_MINT_SA set but TOKEN_MINT_AUDIENCE missing; token minting disabled")
		return nil
	}
	interval := 30 * time.Minute
	if v := os.Getenv("TOKEN_MINT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Printf("warning: invalid TOKEN_MINT_INTERVAL %q, using %s", v, interval)
		} else {
			interval = d
		}
	}
	return &TokenMinter{SA: sa, Audience: aud, Interval: interval}
}

// Mint signs a fresh IAP-compatible JWT via the iamcredentials API,
// authenticated with an access token from the GCE metadata server.
func (m *TokenMinter) Mint() (string, error) {
	accessToken, err := metadataAccessToken()
	if err != nil {
		return "", fmt.Errorf("fetching access token: %w", err)
	}

	now := time.Now().Unix()
	claims, err := json.Marshal(map[string]interface{}{
		"iss": m.SA,
		"sub": m.SA,
		"aud": m.Audience,
		"iat": now,
		"exp": now + int64(tokenLifetime.Seconds()),
	})
	if err != nil {
		return "", fmt.Errorf("marshaling claims: %w", err)
	}
	body, err := json.Marshal(map[string]string{"payload": string(claims)})
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	signURL := fmt.Sprintf("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:signJwt", m.SA)
	req, err := http.NewRequest("POST", signURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("signJwt request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("signJwt returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SignedJwt string `json:"signedJwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding signJwt response: %w", err)
	}
	if result.SignedJwt == "" {
		return "", fmt.Errorf("signJwt response missing signedJwt")
	}
	return result.SignedJwt, nil
}

// metadataAccessToken fetches an access token for the service's runtime
// service account from the GCE metadata server (available on Cloud Run).
func metadataAccessToken() (string, error) {
	req, err := http.NewRequest("GET",
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("metadata server returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("metadata response missing access_token")
	}
	return result.AccessToken, nil
}

// deliverLoop sends a fresh token to the publisher immediately, then
// every Interval, until stop closes or a write fails. It is the only
// writer on the publisher connection after the welcome message, so no
// write lock is needed.
func (m *TokenMinter) deliverLoop(conn *websocket.Conn, stop <-chan struct{}) {
	delay := time.Duration(0) // mint immediately on connect
	for {
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}

		token, err := m.Mint()
		if err != nil {
			log.Printf("token mint failed (retrying in %s): %v", mintRetryDelay, err)
			delay = mintRetryDelay
			continue
		}
		if err := conn.WriteJSON(ControlMessage{Type: "token", Message: token}); err != nil {
			// Connection is gone; the read loop will clean up.
			log.Printf("token delivery failed: %v", err)
			return
		}
		log.Printf("delivered fresh IAP token to publisher (next in %s)", m.Interval)
		delay = m.Interval
	}
}
