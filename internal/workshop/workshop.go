// Package workshop talks to the Steam Workshop for Starbound.
//
// Item details are public (no API key), so the managed mods can always be
// rendered with a title, preview image and subscriber count. Searching the
// Workshop requires a Steam Web API key, which the operator enters in the
// panel.
package workshop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	appID    = "211820"
	apiBase  = "https://api.steampowered.com"
	cacheTTL = 10 * time.Minute
)

// Item is one Workshop entry.
type Item struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Creator       string    `json:"creator"`
	Description   string    `json:"description"`
	Preview       string    `json:"preview"`
	Subscriptions int64     `json:"subscriptions"`
	Favorited     int64     `json:"favorited"`
	Views         int64     `json:"views"`
	Size          int64     `json:"size"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	Tags          []string  `json:"tags"`
	Banned        bool      `json:"banned"`
}

type cacheEntry struct {
	item Item
	at   time.Time
}

// Client fetches Workshop metadata.
type Client struct {
	HTTP   *http.Client
	APIKey string

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New creates a client.
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}, cache: map[string]cacheEntry{}}
}

// Details returns metadata for the given ids, using the in-memory cache.
func (c *Client) Details(ctx context.Context, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return []Item{}, nil
	}

	out := make([]Item, 0, len(ids))
	missing := make([]string, 0, len(ids))
	now := time.Now()

	c.mu.Lock()
	for _, id := range ids {
		if entry, ok := c.cache[id]; ok && now.Sub(entry.at) < cacheTTL {
			out = append(out, entry.item)
			continue
		}
		missing = append(missing, id)
	}
	c.mu.Unlock()

	if len(missing) == 0 {
		return out, nil
	}

	fetched, err := c.fetchDetails(ctx, missing)
	if err != nil {
		if len(out) > 0 {
			return out, nil
		}
		return nil, err
	}

	c.mu.Lock()
	for _, item := range fetched {
		c.cache[item.ID] = cacheEntry{item: item, at: now}
	}
	c.mu.Unlock()

	return append(out, fetched...), nil
}

func (c *Client) fetchDetails(ctx context.Context, ids []string) ([]Item, error) {
	form := url.Values{}
	form.Set("itemcount", strconv.Itoa(len(ids)))
	for i, id := range ids {
		form.Set(fmt.Sprintf("publishedfileids[%d]", i), id)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/ISteamRemoteStorage/GetPublishedFileDetails/v1/",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var payload struct {
		Response struct {
			Details []rawDetails `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := c.do(req, &payload); err != nil {
		return nil, err
	}

	out := make([]Item, 0, len(payload.Response.Details))
	for _, raw := range payload.Response.Details {
		if raw.Result != 1 {
			continue
		}
		out = append(out, raw.item())
	}
	return out, nil
}

// Search queries the Workshop. It requires a Steam Web API key.
func (c *Client) Search(ctx context.Context, query string, page, perPage int) ([]Item, int, error) {
	if c.APIKey == "" {
		return nil, 0, ErrNoAPIKey
	}
	if page < 1 {
		page = 1
	}
	if perPage <= 0 || perPage > 50 {
		perPage = 20
	}

	params := url.Values{}
	params.Set("key", c.APIKey)
	params.Set("page", strconv.Itoa(page))
	params.Set("numperpage", strconv.Itoa(perPage))
	params.Set("creator_appid", appID)
	params.Set("appid", appID)
	params.Set("return_short_description", "true")
	params.Set("return_previews", "true")
	params.Set("return_tags", "true")
	params.Set("query_type", "3")
	if q := strings.TrimSpace(query); q != "" {
		params.Set("search_text", q)
		params.Set("query_type", "11")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/IPublishedFileService/QueryFiles/v1/?"+params.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}

	var payload struct {
		Response struct {
			Total   int          `json:"total"`
			Details []rawDetails `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := c.do(req, &payload); err != nil {
		return nil, 0, err
	}

	out := make([]Item, 0, len(payload.Response.Details))
	for _, raw := range payload.Response.Details {
		if raw.Result != 1 {
			continue
		}
		out = append(out, raw.item())
	}
	return out, payload.Response.Total, nil
}

// ErrNoAPIKey is returned when a search is attempted without a key.
var ErrNoAPIKey = fmt.Errorf("a Steam Web API key is required to search the Workshop")

type rawDetails struct {
	ID            string   `json:"publishedfileid"`
	Result        int      `json:"result"`
	Title         string   `json:"title"`
	Creator       string   `json:"creator"`
	Description   string   `json:"description"`
	ShortDesc     string   `json:"short_description"`
	FileDesc      string   `json:"file_description"`
	Preview       string   `json:"preview_url"`
	Subscriptions int64    `json:"subscriptions"`
	Favorited     int64    `json:"favorited"`
	Views         int64    `json:"views"`
	FileSize      string   `json:"file_size"`
	TimeCreated   int64    `json:"time_created"`
	TimeUpdated   int64    `json:"time_updated"`
	Banned        flexBool `json:"banned"`
	Tags          []struct {
		Tag string `json:"tag"`
	} `json:"tags"`
}

func (r rawDetails) item() Item {
	description := r.Description
	if description == "" {
		description = r.ShortDesc
	}
	if description == "" {
		description = r.FileDesc
	}
	size, _ := strconv.ParseInt(r.FileSize, 10, 64)

	tags := make([]string, 0, len(r.Tags))
	for _, tag := range r.Tags {
		if tag.Tag != "" {
			tags = append(tags, tag.Tag)
		}
	}

	return Item{
		ID:            r.ID,
		Title:         r.Title,
		Creator:       r.Creator,
		Description:   description,
		Preview:       r.Preview,
		Subscriptions: r.Subscriptions,
		Favorited:     r.Favorited,
		Views:         r.Views,
		Size:          size,
		Created:       time.Unix(r.TimeCreated, 0).UTC(),
		Updated:       time.Unix(r.TimeUpdated, 0).UTC(),
		Tags:          tags,
		Banned:        bool(r.Banned),
	}
}

// flexBool accepts the different shapes Steam uses for the "banned" flag:
// GetPublishedFileDetails returns 0/1, QueryFiles returns true/false.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "true", "1":
		*b = true
	case "false", "0", "null", "":
		*b = false
	default:
		return fmt.Errorf("cannot read a boolean from %s", data)
	}
	return nil
}

func (c *Client) do(req *http.Request, dst any) error {
	req.Header.Set("User-Agent", "starbound-panel")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("steam workshop: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(dst)
}
