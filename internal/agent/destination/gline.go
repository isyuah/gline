package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/isyuah/gline/internal/logentry"
)

type GlineDest struct {
	URL    string
	Token  string
	client *http.Client
}

func NewGlineDest(url string, token string) *GlineDest {
	return &GlineDest{URL: url, Token: token, client: &http.Client{
		Timeout: 10 * time.Second,
	}}
}

func prepareRequestBody(entries *[]logentry.LogEntry) (io.Reader, error) {
	body, err := json.Marshal(map[string]any{
		"entries": entries,
	})
	return bytes.NewBuffer(body), err
}

func (g GlineDest) SendEntries(ctx context.Context, entries []logentry.LogEntry) error {
	body, err := prepareRequestBody(&entries)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", g.URL, body)
	if err != nil {
		return fmt.Errorf("error creating request: %s", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gline return unexpected status: %s", resp.Status)
	}
	return nil
}
