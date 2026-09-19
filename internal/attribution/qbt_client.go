package attribution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// qbtClient is a tiny qBittorrent WebUI client. We only need:
//   - login (cookie-based)
//   - GET /api/v2/torrents/info?hashes=H1|H2|... → snapshot for each hash
//
// qB's bulk-info endpoint is the right shape for our join: pass the
// info_hash list from Prowlarr's grab events, get back one record
// per hash currently in qB. Hashes not in qB are simply absent from
// the response — that's how we detect "missing_in_qb."
type qbtClient struct {
	baseURL string
	user    string
	pass    string
	http    *http.Client
	loggedIn bool
}

func newQBTClient(baseURL, user, pass string, timeout time.Duration) (*qbtClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &qbtClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		http:    &http.Client{Timeout: timeout, Jar: jar},
	}, nil
}

// Login posts to /api/v2/auth/login. Cookie is captured by the
// shared cookie jar; subsequent calls use it automatically.
func (c *qbtClient) Login(ctx context.Context) error {
	if c.loggedIn {
		return nil
	}
	form := url.Values{}
	form.Set("username", c.user)
	form.Set("password", c.pass)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.baseURL)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qbt: login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// A 2xx with an empty body means qB bypassed auth for this client
	// (WebUI\AuthSubnetWhitelist / LocalHostAuth) and issued no "Ok.".
	// Rejecting that is what took the evidence qB poller dark for 11
	// days; see checkLoginResponse in internal/evidence/sources/qbpoller.
	// An unauthenticated session still fails loudly on the next call —
	// torrents/info answers 403.
	if trimmed := strings.TrimSpace(string(body)); resp.StatusCode/100 != 2 || (trimmed != "" && trimmed != "Ok.") {
		snip := string(body)
		if len(snip) > 100 {
			snip = snip[:100] + "..."
		}
		return fmt.Errorf("qbt: login failed: status=%d body=%q", resp.StatusCode, snip)
	}
	c.loggedIn = true
	return nil
}

// qbtTorrentInfo is qB's projection. Many fields elided; keep the
// subset we use in the join.
type qbtTorrentInfo struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	Category     string  `json:"category"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	NumSeeds     int     `json:"num_seeds"`
	NumLeechs    int     `json:"num_leechs"`
	Downloaded   int64   `json:"downloaded"`
	UpSpeed      int64   `json:"upspeed"`
	DlSpeed      int64   `json:"dlspeed"`
	LastActivity int64   `json:"last_activity"` // unix
	TrackerErr   string  `json:"tracker_error"` // not always present
}

// LookupHashes returns one QBState per requested info_hash. Hashes
// not currently in qB get a Found=false sentinel.
//
// qB's hashes filter accepts a |-separated list; we batch in groups
// of 100 to keep URLs reasonable.
func (c *qbtClient) LookupHashes(ctx context.Context, hashes []string) (map[string]QBState, error) {
	out := make(map[string]QBState, len(hashes))
	// Pre-populate sentinel rows so missing-from-qB hashes get a
	// Found=false entry without extra logic in the caller.
	for _, h := range hashes {
		hl := strings.ToLower(h)
		out[hl] = QBState{Found: false, Hash: hl}
	}
	if len(hashes) == 0 {
		return out, nil
	}

	const batchSize = 100
	for i := 0; i < len(hashes); i += batchSize {
		end := i + batchSize
		if end > len(hashes) {
			end = len(hashes)
		}
		batch := hashes[i:end]
		filter := strings.Join(batch, "|")
		q := url.Values{}
		q.Set("hashes", filter)
		endpoint := c.baseURL + "/api/v2/torrents/info?" + q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("qbt: info: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("qbt: info http %d", resp.StatusCode)
		}
		var infos []qbtTorrentInfo
		if err := json.Unmarshal(body, &infos); err != nil {
			return nil, fmt.Errorf("qbt: info parse: %w", err)
		}
		for _, t := range infos {
			h := strings.ToLower(t.Hash)
			out[h] = QBState{
				Found:        true,
				Hash:         h,
				Name:         t.Name,
				Category:     t.Category,
				State:        t.State,
				Progress:     t.Progress,
				NumSeeds:     t.NumSeeds,
				NumLeechs:    t.NumLeechs,
				Downloaded:   t.Downloaded,
				UpSpeed:      t.UpSpeed,
				DlSpeed:      t.DlSpeed,
				LastActivity: time.Unix(t.LastActivity, 0),
				TrackerError: t.TrackerErr,
			}
		}
	}
	return out, nil
}
