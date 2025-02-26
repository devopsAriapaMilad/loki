package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type roundTripper struct {
	instanceID    string
	token         string
	injectHeaders map[string][]string
	next          http.RoundTripper
}

func (r *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-Scope-OrgID", r.instanceID)
	if r.token != "" {
		req.SetBasicAuth(r.instanceID, r.token)
	}

	for key, values := range r.injectHeaders {
		for _, v := range values {
			req.Header.Add(key, v)
		}

		fmt.Println(req.Header.Values(key))
	}

	return r.next.RoundTrip(req)
}

type CortexClientOption interface {
	Type() string
}

type InjectHeadersOption map[string][]string

func (n InjectHeadersOption) Type() string {
	return "headerinject"
}

// Client is a HTTP client that adds basic auth and scope
type Client struct {
	Now time.Time

	httpClient *http.Client
	baseURL    string
	instanceID string
}

// NewLogsClient creates a new client
func New(instanceID, token, baseURL string, opts ...CortexClientOption) *Client {
	rt := &roundTripper{
		instanceID: instanceID,
		token:      token,
		next:       http.DefaultTransport,
	}

	for _, opt := range opts {
		switch opt.Type() {
		case "headerinject":
			rt.injectHeaders = opt.(InjectHeadersOption)
		}
	}

	return &Client{
		Now: time.Now(),
		httpClient: &http.Client{
			Transport: rt,
		},
		baseURL:    baseURL,
		instanceID: instanceID,
	}
}

// PushLogLine creates a new logline with the current time as timestamp
func (c *Client) PushLogLine(line string, extraLabels ...map[string]string) error {
	return c.pushLogLine(line, c.Now, extraLabels...)
}

// PushLogLineWithTimestamp creates a new logline at the given timestamp
// The timestamp has to be a Unix timestamp (epoch seconds)
func (c *Client) PushLogLineWithTimestamp(line string, timestamp time.Time, extraLabelList ...map[string]string) error {
	return c.pushLogLine(line, timestamp, extraLabelList...)
}

func formatTS(ts time.Time) string {
	return strconv.FormatInt(ts.UnixNano(), 10)
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

// pushLogLine creates a new logline
func (c *Client) pushLogLine(line string, timestamp time.Time, extraLabelList ...map[string]string) error {
	apiEndpoint := fmt.Sprintf("%s/loki/api/v1/push", c.baseURL)

	s := stream{
		Stream: map[string]string{
			"job": "varlog",
		},
		Values: [][]string{
			{
				formatTS(timestamp),
				line,
			},
		},
	}
	// add extra labels
	for _, labelList := range extraLabelList {
		for k, v := range labelList {
			s.Stream[k] = v
		}
	}

	data, err := json.Marshal(&struct {
		Streams []stream `json:"streams"`
	}{
		Streams: []stream{s},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", apiEndpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scope-OrgID", c.instanceID)

	// Execute HTTP request
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode/100 == 2 {
		defer res.Body.Close()
		return nil
	}

	buf, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("reading request failed with status code %v: %w", res.StatusCode, err)
	}

	return fmt.Errorf("request failed with status code %v: %w", res.StatusCode, errors.New(string(buf)))
}

func (c *Client) Get(path string) (*http.Response, error) {
	url := fmt.Sprintf("%s%s", c.baseURL, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// Get all the metrics
func (c *Client) Metrics() (string, error) {
	url := fmt.Sprintf("%s/metrics", c.baseURL)
	res, err := http.Get(url)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	if _, err := io.Copy(&sb, res.Body); err != nil {
		return "", err
	}

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request failed with status code %d", res.StatusCode)
	}
	return sb.String(), nil
}

// Flush all in-memory chunks held by the ingesters to the backing store
func (c *Client) Flush() error {
	req, err := c.request("POST", fmt.Sprintf("%s/flush", c.baseURL))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("request failed with status code %d", res.StatusCode)
}

// StreamValues holds a label key value pairs for the Stream and a list of a list of values
type StreamValues struct {
	Stream map[string]string
	Values [][]string
}

// MatrixValues holds a label key value pairs for the metric and a list of a list of values
type MatrixValues struct {
	Metric map[string]string
	Values [][]interface{}
}

// VectorValues holds a label key value pairs for the metric and single timestamp and value
type VectorValues struct {
	Metric map[string]string `json:"metric"`
	Time   time.Time
	Value  string
}

func (a *VectorValues) UnmarshalJSON(b []byte) error {
	var s struct {
		Metric map[string]string `json:"metric"`
		Value  []interface{}     `json:"value"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	a.Metric = s.Metric
	if len(s.Value) != 2 {
		return fmt.Errorf("unexpected value length %d", len(s.Value))
	}
	if ts, ok := s.Value[0].(int64); ok {
		a.Time = time.Unix(ts, 0)
	}
	if val, ok := s.Value[1].(string); ok {
		a.Value = val
	}
	return nil
}

// DataType holds the result type and a list of StreamValues
type DataType struct {
	ResultType string
	Stream     []StreamValues
	Matrix     []MatrixValues
	Vector     []VectorValues
}

func (a *DataType) UnmarshalJSON(b []byte) error {
	// get the result type
	var s struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	switch s.ResultType {
	case "streams":
		if err := json.Unmarshal(s.Result, &a.Stream); err != nil {
			return err
		}
	case "matrix":
		if err := json.Unmarshal(s.Result, &a.Matrix); err != nil {
			return err
		}
	case "vector":
		if err := json.Unmarshal(s.Result, &a.Vector); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown result type %s", s.ResultType)
	}
	a.ResultType = s.ResultType
	return nil
}

// Response holds the status and data
type Response struct {
	Status string
	Data   DataType
}

// RunRangeQuery runs a query and returns an error if anything went wrong
func (c *Client) RunRangeQuery(query string) (*Response, error) {
	buf, statusCode, err := c.run(c.rangeQueryURL(query))
	if err != nil {
		return nil, err
	}

	return c.parseResponse(buf, statusCode)
}

// RunQuery runs a query and returns an error if anything went wrong
func (c *Client) RunQuery(query string) (*Response, error) {
	v := url.Values{}
	v.Set("query", query)
	v.Set("time", formatTS(c.Now.Add(time.Second)))

	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = "/loki/api/v1/query"
	u.RawQuery = v.Encode()

	buf, statusCode, err := c.run(u.String())
	if err != nil {
		return nil, err
	}

	return c.parseResponse(buf, statusCode)
}

func (c *Client) parseResponse(buf []byte, statusCode int) (*Response, error) {
	lokiResp := Response{}
	err := json.Unmarshal(buf, &lokiResp)
	if err != nil {
		return nil, fmt.Errorf("error parsing response data: %w", err)
	}

	if statusCode/100 == 2 {
		return &lokiResp, nil
	}
	return nil, fmt.Errorf("request failed with status code %d: %w", statusCode, errors.New(string(buf)))
}

func (c *Client) rangeQueryURL(query string) string {
	v := url.Values{}
	v.Set("query", query)
	v.Set("start", formatTS(c.Now.Add(-2*time.Hour)))
	v.Set("end", formatTS(c.Now.Add(time.Second)))

	u, err := url.Parse(c.baseURL)
	if err != nil {
		panic(err)
	}
	u.Path = "/loki/api/v1/query_range"
	u.RawQuery = v.Encode()

	return u.String()
}

func (c *Client) LabelNames() ([]string, error) {
	url := fmt.Sprintf("%s/loki/api/v1/labels", c.baseURL)

	req, err := c.request("GET", url)
	if err != nil {
		return nil, err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Unexpected status code of %d", res.StatusCode)
	}

	var values struct {
		Data []string `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&values); err != nil {
		return nil, err
	}

	return values.Data, nil
}

// LabelValues return a LabelValues query
func (c *Client) LabelValues(labelName string) ([]string, error) {
	url := fmt.Sprintf("%s/loki/api/v1/label/%s/values", c.baseURL, url.PathEscape(labelName))

	req, err := c.request("GET", url)
	if err != nil {
		return nil, err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Unexpected status code of %d", res.StatusCode)
	}

	var values struct {
		Data []string `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&values); err != nil {
		return nil, err
	}

	return values.Data, nil
}

func (c *Client) request(method string, url string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Scope-OrgID", c.instanceID)
	return req, nil
}

func (c *Client) run(u string) ([]byte, int, error) {
	req, err := c.request("GET", u)
	if err != nil {
		return nil, 0, err
	}

	// Execute HTTP request
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()

	buf, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed with status code %v: %w", res.StatusCode, err)
	}

	return buf, res.StatusCode, nil
}
