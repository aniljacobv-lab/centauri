// Package centauri is a thin, zero-dependency Go client for a Centauri
// server. It mirrors the Python SDK's surface — one method per capability,
// named for what you want — over the HTTP/JSON API.
//
//	c := centauri.New("http://localhost:7771", centauri.WithToken("secret"))
//	c.Add("toy:robot", map[string]any{"price_cents": 500})
//	facts, _ := c.Get("toy:robot")                  // what's true now
//	res, _  := c.Query("FACTS OF toy:robot WHY")    // any CeQL
//	c.Run("reprice", map[string]any{"item": "toy:robot", "pct": 90})
//
// Standard library only — net/http + encoding/json. Drop it in any module.
package centauri

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Event is one bi-temporal fact. Zero fields are omitted on the wire, so the
// server fills sensible defaults (facet "source", type "OBSERVED", now).
type Event struct {
	EventID       string         `json:"event_id,omitempty"`
	Subject       string         `json:"subject"`
	Facet         string         `json:"facet,omitempty"`
	Type          string         `json:"type,omitempty"`
	Value         map[string]any `json:"value,omitempty"`
	EffectiveTime int64          `json:"effective_time,omitempty"`
	RecordedTime  int64          `json:"recorded_time,omitempty"`
	Confidence    float64        `json:"confidence,omitempty"`
	Provenance    string         `json:"provenance,omitempty"`
	SourceSystem  string         `json:"source_system,omitempty"`
	SourceRef     string         `json:"source_ref,omitempty"`
	SchemaID      string         `json:"schema_id,omitempty"`
}

// Client is a connection to one Centauri server.
type Client struct {
	URL      string
	Token    string
	Database string
	HTTP     *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithToken sets the bearer token sent on every request.
func WithToken(t string) Option { return func(c *Client) { c.Token = t } }

// WithDatabase selects a named environment (the ?db= parameter).
func WithDatabase(db string) Option { return func(c *Client) { c.Database = db } }

// WithHTTPClient supplies a custom *http.Client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.HTTP = h } }

// New creates a client for the server at url.
func New(url string, opts ...Option) *Client {
	c := &Client{URL: url, HTTP: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) params(extra url.Values) string {
	if extra == nil {
		extra = url.Values{}
	}
	if c.Database != "" {
		extra.Set("db", c.Database)
	}
	if s := extra.Encode(); s != "" {
		return "?" + s
	}
	return ""
}

func (c *Client) do(method, path string, q url.Values, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.URL+path+c.params(q), rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error != "" {
			return fmt.Errorf("centauri: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("centauri: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Query runs any CeQL statement and returns the raw result map (kind + data).
// It's the escape hatch: anything you can type in the shell, you can run here.
func (c *Client) Query(ceql string) (map[string]any, error) {
	var out map[string]any
	err := c.do("POST", "/v1/query", nil, map[string]string{"q": ceql}, &out)
	return out, err
}

// Append writes facts in one atomic batch and returns their ids.
func (c *Client) Append(events ...Event) ([]string, error) {
	var out struct {
		Appended []string `json:"appended"`
	}
	err := c.do("POST", "/v1/append", nil, map[string]any{"events": events}, &out)
	return out.Appended, err
}

// Add saves one fact (defaults: facet "source", type OBSERVED, confidence 1)
// and returns its new id. Insert and update are the same act in Centauri.
func (c *Client) Add(subject string, value map[string]any) (string, error) {
	ids, err := c.Append(Event{Subject: subject, Facet: "source", Type: "OBSERVED",
		Value: value, Confidence: 1, Provenance: "HUMAN_ENTRY", SourceSystem: "go-sdk"})
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}

// Get returns the current facts about a subject (one per facet).
func (c *Client) Get(subject string) ([]Event, error) {
	var out []Event
	err := c.do("GET", "/v1/current", url.Values{"subject": {subject}}, nil, &out)
	return out, err
}

// At time-travels: what was true at `at` (UnixMicro)? known>0 replays what
// the database believed at that transaction time. Pass 0 to omit either.
func (c *Client) At(subject string, at, known int64) ([]Event, error) {
	q := url.Values{"subject": {subject}}
	if at > 0 {
		q.Set("at", strconv.FormatInt(at, 10))
	}
	if known > 0 {
		q.Set("known", strconv.FormatInt(known, 10))
	}
	var out []Event
	err := c.do("GET", "/v1/asof", q, nil, &out)
	return out, err
}

// History returns every fact ever recorded about a subject, in time order.
func (c *Client) History(subject string) ([]Event, error) {
	var out []Event
	err := c.do("GET", "/v1/history", url.Values{"subject": {subject}}, nil, &out)
	return out, err
}

// Context returns everything about a subject in one call (facts, history,
// causes, disagreements, enrichments, confidence) — the agent bundle.
func (c *Client) Context(subject string) (map[string]any, error) {
	var out map[string]any
	err := c.do("GET", "/v1/context", url.Values{"subject": {subject}}, nil, &out)
	return out, err
}

// DefineProcedure stores a CePL procedure (versioned, append-only).
func (c *Client) DefineProcedure(source string) (map[string]any, error) {
	var out map[string]any
	err := c.do("POST", "/v1/proc", nil, map[string]string{"source": source}, &out)
	return out, err
}

// Run executes a stored CePL procedure; the result includes a step trace.
func (c *Client) Run(name string, args map[string]any) (map[string]any, error) {
	var out map[string]any
	err := c.do("POST", "/v1/proc/run", nil, map[string]any{"name": name, "args": args}, &out)
	return out, err
}

// Changes is the CDC tail: facts committed at/after the byte cursor, plus the
// next cursor to resume from. Page until caught_up.
func (c *Client) Changes(from int64) (events []Event, cursor int64, caughtUp bool, err error) {
	var out struct {
		Events   []Event `json:"events"`
		Cursor   int64   `json:"cursor"`
		CaughtUp bool    `json:"caught_up"`
	}
	err = c.do("GET", "/v1/changes", url.Values{"from": {strconv.FormatInt(from, 10)}}, nil, &out)
	return out.Events, out.Cursor, out.CaughtUp, err
}

// Stats returns store counters (events, subjects, open, pending, links).
func (c *Client) Stats() (map[string]int, error) {
	var out map[string]int
	err := c.do("GET", "/v1/stats", nil, nil, &out)
	return out, err
}

// Subjects lists every subject the database knows.
func (c *Client) Subjects() ([]string, error) {
	var out []string
	err := c.do("GET", "/v1/subjects", nil, nil, &out)
	return out, err
}
