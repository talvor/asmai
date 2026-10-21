// SPDX-License-Identifier: Apache-2.0

package lavish

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
)

// Client is the daemon's connection to the factory's Lavish server: the one
// that opens a review page, listens for what the user sends from it and ends
// it. No agent runs Lavish or talks to this API (04 rule 22).
type Client struct {
	// Port is the port the server listens on.
	Port int
	// HTTP is the client to use; with none, one that never times out a
	// request, as listening for the user takes as long as the user does.
	HTTP *http.Client
}

// ErrNoPage is what Lavish answers when it has no such page, as after its
// state directory was lost.
var ErrNoPage = errors.New("Lavish has no such page")

// Owner names the listener the daemon is, as Lavish shows it.
const Owner = "asmai"

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, URL(c.Port)+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.client().Do(req)
}

// Opened is a page opened in Lavish.
type Opened struct {
	// URL is the link the user opens.
	URL string `json:"url"`
	// Status is "opened", or "user-ended" when the user ended the page's
	// session and it was not reopened.
	Status string `json:"status"`
}

// OpenPage opens the page in file as a review page and returns its link.
// The file is an absolute path of a file that exists. A page the user ended
// is reopened: the daemon opens a page for a version that is still pending,
// which the user's ending its session does not settle.
func (c Client) OpenPage(ctx context.Context, file string) (Opened, error) {
	resp, err := c.do(ctx, http.MethodPost, "/api/sessions", map[string]any{"file": file, "reopen": true})
	if err != nil {
		return Opened{}, fmt.Errorf("opening the Lavish page %s: %w", file, err)
	}
	defer resp.Body.Close()
	var opened Opened
	if err := decode(resp, &opened); err != nil {
		return Opened{}, fmt.Errorf("opening the Lavish page %s: %w", file, err)
	}
	if opened.URL == "" {
		return Opened{}, fmt.Errorf("opening the Lavish page %s: Lavish gave no link", file)
	}
	return opened, nil
}

// EndPage ends the Lavish session of the page in file: the daemon's own end
// of a page whose version is settled.
func (c Client) EndPage(ctx context.Context, file string) error {
	resp, err := c.do(ctx, http.MethodPost, "/api/end", map[string]any{"file": file})
	if err != nil {
		return fmt.Errorf("ending the Lavish page %s: %w", file, err)
	}
	defer resp.Body.Close()
	if err := decode(resp, &struct{}{}); err != nil {
		return fmt.Errorf("ending the Lavish page %s: %w", file, err)
	}
	return nil
}

// Feedback is what listening to a page returned.
type Feedback struct {
	// Status is "feedback" when the user sent something, "ended" when the
	// session ended with nothing more to send, and "waiting" when the wait
	// ran out.
	Status string `json:"status"`
	// Prompts is what the user sent, each exactly as Lavish delivered it.
	Prompts []json.RawMessage `json:"prompts"`
	// SessionEnded says the user ended the session in sending it, and
	// EndedBy who ended it.
	SessionEnded bool   `json:"session_ended"`
	EndedBy      string `json:"ended_by"`
	// Code is Lavish's reason when another listener holds the page.
	Code string `json:"code"`
}

// Poll listens to the page in file as Lavish's own agents do: it returns what
// the user sends, as soon as they send it. It waits as long as ctx lives, or
// as timeoutMs when that is positive; collected prompts are Lavish's to hand
// over once.
func (c Client) Poll(ctx context.Context, file string, timeoutMs int) (Feedback, error) {
	query := url.Values{"file": {file}, "owner": {Owner}}
	if timeoutMs > 0 {
		query.Set("timeoutMs", fmt.Sprint(timeoutMs))
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/poll?"+query.Encode(), nil)
	if err != nil {
		return Feedback{}, fmt.Errorf("listening to the Lavish page %s: %w", file, err)
	}
	defer resp.Body.Close()
	var f Feedback
	if err := decode(resp, &f); err != nil {
		return Feedback{}, fmt.Errorf("listening to the Lavish page %s: %w", file, err)
	}
	if f.Code != "" {
		return Feedback{}, fmt.Errorf("listening to the Lavish page %s: %s", file, f.Code)
	}
	return f, nil
}

// decode reads the JSON a Lavish endpoint answered, which a listening poll
// starts with heartbeat blanks.
func decode(resp *http.Response, into any) error {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	data = bytes.TrimSpace(data)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoPage
	}
	if resp.StatusCode/100 != 2 {
		var failure struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(data, &failure) == nil && (failure.Error != "" || failure.Code != "") {
			return fmt.Errorf("Lavish answered %s: %s", resp.Status, strings.TrimSpace(failure.Code+" "+failure.Error))
		}
		return fmt.Errorf("Lavish answered %s", resp.Status)
	}
	if len(data) == 0 {
		return errors.New("Lavish ended its answer before it began")
	}
	return json.Unmarshal(data, into)
}
