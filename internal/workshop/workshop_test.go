package workshop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRawDetailsItem(t *testing.T) {
	raw := rawDetails{
		ID:            "2056569194",
		Result:        1,
		Title:         "Mini Health Bar [remastered]",
		Description:   "This mod adds health bars.",
		Preview:       "https://images.steamusercontent.com/ugc/1/2/",
		Subscriptions: 131,
		Favorited:     141,
		Views:         4069,
		FileSize:      "385342",
		TimeCreated:   1586594793,
		TimeUpdated:   1586610506,
	}
	raw.Tags = append(raw.Tags, struct {
		Tag string `json:"tag"`
	}{Tag: "User Interface"})

	item := raw.item()
	if item.ID != "2056569194" || item.Title == "" || item.Description == "" {
		t.Fatalf("unexpected item: %+v", item)
	}
	if item.Size != 385342 || item.Subscriptions != 131 {
		t.Fatalf("size/subscriptions: %+v", item)
	}
	if len(item.Tags) != 1 || item.Tags[0] != "User Interface" {
		t.Fatalf("tags: %+v", item.Tags)
	}
	if item.Created.UTC().Year() != time.Unix(1586594793, 0).UTC().Year() {
		t.Fatalf("created: %v", item.Created)
	}
}

func TestSearchRequiresKey(t *testing.T) {
	c := New()
	if _, _, err := c.Search(context.Background(), "health", 1, 20); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("expected ErrNoAPIKey, got %v", err)
	}
}

func TestDetailsEmptyList(t *testing.T) {
	c := New()
	items, err := c.Details(context.Background(), nil)
	if err != nil || len(items) != 0 {
		t.Fatalf("unexpected result: %v %v", items, err)
	}
}
func TestRawDetailsAcceptsBothBannedShapes(t *testing.T) {
	cases := []string{
		`{"publishedfileid":"1","result":1,"banned":false,"title":"a"}`,
		`{"publishedfileid":"1","result":1,"banned":true,"title":"a"}`,
		`{"publishedfileid":"1","result":1,"banned":0,"title":"a"}`,
		`{"publishedfileid":"1","result":1,"banned":1,"title":"a"}`,
	}
	for _, raw := range cases {
		var details rawDetails
		if err := json.Unmarshal([]byte(raw), &details); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
}
func TestDecodeRealQueryFilesResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/query.json")
	if err != nil {
		t.Skip("fixture not present")
	}

	var payload struct {
		Response struct {
			Total   int          `json:"total"`
			Details []rawDetails `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Response.Total == 0 || len(payload.Response.Details) == 0 {
		t.Fatalf("empty response: %+v", payload.Response)
	}
	for _, details := range payload.Response.Details {
		if details.ID == "" || details.Title == "" {
			t.Fatalf("incomplete item: %+v", details)
		}
		_ = details.item()
	}
}
