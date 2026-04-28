package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Telegram Bot API types used by the bridge. Only the subset we need.

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private" | "group" | ...
}

type PhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int    `json:"file_size,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int    `json:"file_size,omitempty"`
}

type Message struct {
	MessageID int         `json:"message_id"`
	From      *User       `json:"from"`
	Chat      Chat        `json:"chat"`
	Date      int64       `json:"date"`
	Text      string      `json:"text,omitempty"`
	Caption   string      `json:"caption,omitempty"`
	Photo     []PhotoSize `json:"photo,omitempty"`
	Document  *Document   `json:"document,omitempty"`
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
	Edited   *Message `json:"edited_message"`
}

type File struct {
	FileID   string `json:"file_id"`
	FilePath string `json:"file_path"`
	FileSize int    `json:"file_size,omitempty"`
}

// apiResponse is Telegram's envelope.
type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      T      `json:"result"`
}

// Client is a minimal Telegram Bot API client.
type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 0}}
}

func (c *Client) baseURL() string { return "https://api.telegram.org/bot" + c.token }

// GetMe verifies the token and returns the bot's own User.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var resp apiResponse[User]
	if err := c.call(ctx, "getMe", nil, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("getMe: %s", resp.Description)
	}
	return &resp.Result, nil
}

// GetUpdates polls for new updates since offset with a long-poll timeout.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	q := url.Values{}
	if offset != 0 {
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	q.Set("timeout", strconv.Itoa(timeoutSec))
	q.Set("allowed_updates", `["message","edited_message"]`)

	var resp apiResponse[[]Update]
	if err := c.call(ctx, "getUpdates?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("getUpdates: %s", resp.Description)
	}
	return resp.Result, nil
}

// SendMessage sends a plain-text reply.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int) error {
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyTo > 0 {
		body["reply_to_message_id"] = replyTo
	}
	var resp apiResponse[json.RawMessage]
	if err := c.call(ctx, "sendMessage", body, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("sendMessage: %s", resp.Description)
	}
	return nil
}

// SendChatAction keeps the "typing..." indicator alive. Call every ~4s.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	body := map[string]any{"chat_id": chatID, "action": action}
	var resp apiResponse[json.RawMessage]
	_ = c.call(ctx, "sendChatAction", body, &resp) // ignore errors; it's advisory
	return nil
}

// SendPhoto uploads a local image file as a Telegram photo. Telegram
// re-encodes / scales photos for inline preview; use SendDocument
// when the recipient needs the original bytes.
func (c *Client) SendPhoto(ctx context.Context, chatID int64, path, caption string) error {
	f, err := openFile(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("photo", lastPathElem(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL()+"/sendPhoto", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendPhoto http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// SendDocument uploads a local file as a document attachment.
func (c *Client) SendDocument(ctx context.Context, chatID int64, path, caption string) error {
	f, err := openFile(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("document", lastPathElem(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL()+"/sendDocument", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendDocument http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// GetFile resolves a file_id to a downloadable path.
func (c *Client) GetFile(ctx context.Context, fileID string) (*File, error) {
	q := url.Values{}
	q.Set("file_id", fileID)
	var resp apiResponse[File]
	if err := c.call(ctx, "getFile?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("getFile: %s", resp.Description)
	}
	return &resp.Result, nil
}

// DownloadFile downloads the file at filePath (from GetFile) into memory.
func (c *Client) DownloadFile(ctx context.Context, filePath string) ([]byte, error) {
	u := "https://api.telegram.org/file/bot" + c.token + "/" + filePath
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download %s: http %d", filePath, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// call issues a GET or POST request depending on whether body is nil.
func (c *Client) call(ctx context.Context, endpoint string, body map[string]any, out any) error {
	var req *http.Request
	var err error
	if body == nil {
		req, err = http.NewRequestWithContext(ctx, "GET", c.baseURL()+"/"+endpoint, nil)
	} else {
		b, _ := json.Marshal(body)
		req, err = http.NewRequestWithContext(ctx, "POST", c.baseURL()+"/"+endpoint, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("content-type", "application/json")
		}
	}
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Telegram returns 200 with ok=false for logical errors; read either way.
	respBody, _ := io.ReadAll(resp.Body)
	if len(respBody) == 0 {
		return fmt.Errorf("%s: empty response (status %d)", endpoint, resp.StatusCode)
	}
	return json.Unmarshal(respBody, out)
}

// small helpers kept here so api.go has no other deps.
func lastPathElem(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}

func openFile(path string) (io.ReadCloser, error) {
	return osOpen(path)
}

// overridden in tests.
var osOpen = defaultOpen

// Sleep is a package-level hook so tests can stub it.
var sleep = func(d time.Duration) { time.Sleep(d) }
