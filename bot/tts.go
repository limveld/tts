package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TTS is the subset of the TTS server the router drives (an interface so tests
// can substitute a fake).
type TTS interface {
	Say(text, code string) error
	SFX(name string) error
	Pause() error
	Resume() error
	Clear() error
	Skip() error
}

// TTSClient talks to the TTS server's HTTP API.
type TTSClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewTTSClient(baseURL, token string) *TTSClient {
	return &TTSClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *TTSClient) Say(text, code string) error {
	body, _ := json.Marshal(map[string]string{"text": text, "code": code})
	return c.post("/say", body)
}

func (c *TTSClient) SFX(name string) error {
	body, _ := json.Marshal(map[string]string{"name": name})
	return c.post("/sfx", body)
}

func (c *TTSClient) Pause() error  { return c.post("/pause", nil) }
func (c *TTSClient) Resume() error { return c.post("/resume", nil) }
func (c *TTSClient) Clear() error  { return c.post("/clear", nil) }
func (c *TTSClient) Skip() error   { return c.post("/skip", nil) }

func (c *TTSClient) post(path string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s -> %s", path, resp.Status)
	}
	return nil
}
