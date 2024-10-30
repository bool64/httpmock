package httpmock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swaggest/assertjson"
	"github.com/swaggest/assertjson/json5"
)

// Client keeps state of expectations.
type Client struct {
	ConcurrencyLevel int
	JSONComparer     assertjson.Comparer
	OnBodyMismatch   func(received []byte) // Optional, called when received body does not match expected.
	Transport        http.RoundTripper

	baseURL string

	// Headers are default headers added to all requests, can be overridden by WithHeader.
	Headers map[string]string

	// Cookies are default cookies added to all requests, can be overridden by WithCookie.
	Cookies map[string]string

	ctx context.Context //nolint:containedctx // Context is configured separately.

	req      *http.Request
	resp     *http.Response
	respBody []byte

	attempt     int
	retryDelays []time.Duration

	reqHeaders        map[string]string
	reqCookies        map[string]string
	reqQueryParams    url.Values
	reqFormDataParams url.Values
	reqBody           []byte
	reqMethod         string
	reqURI            string

	// reqConcurrency is a number of simultaneous requests to send.
	reqConcurrency int

	retryBackOff    RetryBackOff
	followRedirects bool

	otherRespBody     []byte
	otherResp         *http.Response
	otherRespExpected bool
}

// RetryBackOff defines retry strategy.
//
// This interface matches github.com/cenkalti/retryBackOff/v4.BackOff.
type RetryBackOff interface {
	// NextBackOff returns the duration to wait before retrying the operation,
	// or -1 to indicate that no more retries should be made.
	//
	// Example usage:
	//
	// 	duration := retryBackOff.NextBackOff();
	// 	if (duration == retryBackOff.Stop) {
	// 		// Do not retry operation.
	// 	} else {
	// 		// Sleep for duration and retry operation.
	// 	}
	//
	NextBackOff() time.Duration
}

// RetryBackOffFunc implements RetryBackOff with a function.
type RetryBackOffFunc func() time.Duration

// NextBackOff returns the duration to wait before retrying the operation,
// or -1 to indicate that no more retries should be made.
func (r RetryBackOffFunc) NextBackOff() time.Duration {
	return r()
}

var (
	errEmptyBody                = errors.New("received empty body")
	errResponseCardinality      = errors.New("response status cardinality too high")
	errUnexpectedBody           = errors.New("unexpected body")
	errUnexpectedResponseStatus = errors.New("unexpected response status")
	errOperationNotIdempotent   = errors.New("operation is not idempotent")
	errNoOtherResponses         = errors.New("all responses have same status, no other responses")
)

const defaultConcurrencyLevel = 10

// NewClient creates client instance, baseURL may be empty if Client.SetBaseURL is used later.
func NewClient(baseURL string) *Client {
	c := &Client{
		baseURL:      baseURL,
		JSONComparer: assertjson.Comparer{IgnoreDiff: assertjson.IgnoreDiff},
	}

	c.Reset()

	if baseURL != "" {
		c.SetBaseURL(baseURL)
	}

	return c
}

// HTTPValue contains information about request and response.
type HTTPValue struct {
	Req     *http.Request
	ReqBody []byte

	Resp     *http.Response
	RespBody []byte

	OtherResp     *http.Response
	OtherRespBody []byte

	Attempt     int
	RetryDelays []time.Duration
}

// Details returns HTTP request and response information of last run.
func (c *Client) Details() HTTPValue {
	return HTTPValue{
		Req:     c.req,
		ReqBody: c.reqBody,

		Resp:     c.resp,
		RespBody: c.respBody,

		OtherResp:     c.otherResp,
		OtherRespBody: c.otherRespBody,

		Attempt:     c.attempt,
		RetryDelays: c.retryDelays,
	}
}

// SetBaseURL changes baseURL configured with constructor.
func (c *Client) SetBaseURL(baseURL string) {
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}

	c.baseURL = baseURL
}

// Reset deletes client state.
func (c *Client) Reset() *Client {
	c.ctx = context.Background()

	c.reqHeaders = map[string]string{}
	c.reqCookies = map[string]string{}
	c.reqQueryParams = map[string][]string{}
	c.reqFormDataParams = map[string][]string{}

	c.req = nil

	c.resp = nil
	c.respBody = nil

	c.reqMethod = ""
	c.reqURI = ""
	c.reqBody = nil

	c.reqConcurrency = 0
	c.followRedirects = false
	c.retryBackOff = nil

	c.otherResp = nil
	c.otherRespBody = nil
	c.otherRespExpected = false

	c.attempt = 0
	c.retryDelays = nil

	return c
}

// Fork checks ctx for an existing clone of this Client.
// If one is found, it is returned together with unmodified context.
// Otherwise, a clone of Client is created and put into a new derived context,
// then both new context and cloned Client are returned.
//
// This method enables context-driven concurrent access to shared base Client.
func (c *Client) Fork(ctx context.Context) (context.Context, *Client) {
	// Pointer to current Client is used as context key
	// to enable multiple different clients in same context.
	if fc, ok := ctx.Value(c).(*Client); ok {
		return ctx, fc
	}

	// Making a copy of this Client.
	cc := *c
	fc := &cc
	fc.JSONComparer = c.JSONComparer
	ctx, fc.JSONComparer.Vars = c.JSONComparer.Vars.Fork(ctx)
	ctx = context.WithValue(ctx, c, fc)

	fc.Reset().WithContext(ctx)

	return ctx, fc
}

// FollowRedirects enables automatic following of Location header.
func (c *Client) FollowRedirects() *Client {
	c.followRedirects = true

	return c
}

// AllowRetries allows sending multiple requests until first response assertion passes.
func (c *Client) AllowRetries(b RetryBackOff) *Client {
	c.retryBackOff = b

	return c
}

// WithContext adds context to request.
func (c *Client) WithContext(ctx context.Context) *Client {
	c.ctx = ctx

	return c
}

// WithMethod sets request HTTP method.
func (c *Client) WithMethod(method string) *Client {
	c.reqMethod = method

	return c
}

// WithPath sets request URI path.
//
// Deprecated: use WithURI.
func (c *Client) WithPath(path string) *Client {
	c.reqURI = path

	return c
}

// WithURI sets request URI.
func (c *Client) WithURI(uri string) *Client {
	c.reqURI = uri

	return c
}

// WithBody sets request body.
func (c *Client) WithBody(body []byte) *Client {
	c.reqBody = body

	return c
}

// WithContentType sets request content type.
func (c *Client) WithContentType(contentType string) *Client {
	c.reqHeaders["Content-Type"] = contentType

	return c
}

// WithHeader sets request header.
func (c *Client) WithHeader(key, value string) *Client {
	c.reqHeaders[http.CanonicalHeaderKey(key)] = value

	return c
}

// WithCookie sets request cookie.
func (c *Client) WithCookie(name, value string) *Client {
	c.reqCookies[name] = value

	return c
}

// WithQueryParam appends request query parameter.
func (c *Client) WithQueryParam(name, value string) *Client {
	c.reqQueryParams[name] = append(c.reqQueryParams[name], value)

	return c
}

// WithURLEncodedFormDataParam appends request form data parameter.
func (c *Client) WithURLEncodedFormDataParam(name, value string) *Client {
	c.reqFormDataParams[name] = append(c.reqFormDataParams[name], value)

	return c
}

func (c *Client) do() (err error) { //nolint:funlen
	c.attempt++

	if c.reqConcurrency < 1 {
		c.reqConcurrency = 1
	}

	// A map of responses count by status code.
	statusCodeCount := make(map[int]int, 2)
	wg := sync.WaitGroup{}
	mu := sync.Mutex{}
	resps := make(map[int]*http.Response, 2)
	bodies := make(map[int][]byte, 2)

	for i := 0; i < c.reqConcurrency; i++ {
		wg.Add(1)

		go func() {
			var er error

			defer func() {
				if er != nil {
					mu.Lock()
					err = er
					mu.Unlock()
				}

				wg.Done()
			}()

			req, resp, er := c.doOnce()
			if er != nil {
				return
			}

			body, er := ioutil.ReadAll(resp.Body)
			if er != nil {
				return
			}

			er = resp.Body.Close()
			if er != nil {
				return
			}

			mu.Lock()
			if c.req == nil {
				c.req = req
			}

			if _, ok := statusCodeCount[resp.StatusCode]; !ok {
				resps[resp.StatusCode] = resp
				bodies[resp.StatusCode] = body
				statusCodeCount[resp.StatusCode] = 1
			} else {
				statusCodeCount[resp.StatusCode]++
			}
			mu.Unlock()
		}()
	}

	wg.Wait()

	if err != nil {
		return err
	}

	return c.checkResponses(statusCodeCount, bodies, resps)
}

func (c *Client) expectResp(check func() error) (err error) {
	if c.resp != nil {
		return check()
	}

	if len(c.reqBody) == 0 && len(c.reqFormDataParams) > 0 {
		c.reqBody = []byte(c.reqFormDataParams.Encode())

		if c.reqMethod == "" {
			c.reqMethod = http.MethodPost
		}
	}

	if c.retryBackOff != nil {
		for {
			if err = c.do(); err == nil {
				if err = check(); err == nil {
					return nil
				}
			}

			dur := c.retryBackOff.NextBackOff()

			if dur == -1 {
				return err
			}

			c.retryDelays = append(c.retryDelays, dur)

			time.Sleep(dur)
		}
	}

	if err := c.do(); err != nil {
		return err
	}

	return check()
}

// CheckResponses checks if responses qualify idempotence criteria.
//
// Operation is considered idempotent in one of two cases:
//   - all responses have same status code (e.g. GET /resource: all 200 OK),
//   - all responses but one have same status code (e.g. POST /resource: one 200 OK, many 409 Conflict).
//
// Any other case is considered an idempotence violation.
func (c *Client) checkResponses(
	statusCodeCount map[int]int,
	bodies map[int][]byte,
	resps map[int]*http.Response,
) error {
	var (
		statusCode      int
		otherStatusCode int
	)

	switch {
	case len(statusCodeCount) == 1:
		for code := range statusCodeCount {
			statusCode = code

			break
		}
	case len(statusCodeCount) > 1:
		for code, cnt := range statusCodeCount {
			if cnt == 1 {
				statusCode = code
			} else {
				otherStatusCode = code
			}
		}
	default:
		return fmt.Errorf("%w: %v", errResponseCardinality, statusCodeCount)
	}

	if statusCode == 0 {
		responses := ""
		for c, b := range bodies {
			responses += fmt.Sprintf("\nstatus %d with %d responses, sample body: %s",
				c, statusCodeCount[c], strings.Trim(string(b), "\n"))
		}

		return fmt.Errorf("%w: %v", errOperationNotIdempotent, responses)
	}

	c.resp = resps[statusCode]
	c.respBody = bodies[statusCode]

	if otherStatusCode != 0 {
		c.otherResp = resps[otherStatusCode]
		c.otherRespBody = bodies[otherStatusCode]
	}

	return nil
}

func (c *Client) buildURI() (string, error) {
	uri := c.baseURL + c.reqURI

	if len(c.reqQueryParams) > 0 {
		u, err := url.Parse(uri)
		if err != nil {
			return "", fmt.Errorf("failed to parse requrst uri %s: %w", uri, err)
		}

		q := u.Query()
		for k, v := range c.reqQueryParams {
			q[k] = append(q[k], v...)
		}

		u.RawQuery = q.Encode()

		uri = u.String()
	}

	return uri, nil
}

type readSeekNopCloser struct {
	io.ReadSeeker
}

func (r *readSeekNopCloser) Close() error {
	return nil
}

func (c *Client) buildBody() io.ReadSeekCloser {
	if len(c.reqBody) > 0 {
		return &readSeekNopCloser{ReadSeeker: bytes.NewReader(c.reqBody)}
	}

	return nil
}

func (c *Client) applyHeaders(req *http.Request) {
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}

	for k, v := range c.reqHeaders {
		req.Header.Set(k, v)
	}

	if len(c.reqFormDataParams) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
}

func (c *Client) applyCookies(req *http.Request) {
	cookies := make([]http.Cookie, 0, len(c.Cookies)+len(c.reqCookies))

	for n, v := range c.Cookies {
		if _, found := c.reqCookies[n]; found {
			continue
		}

		cookies = append(cookies, http.Cookie{Name: n, Value: v})
	}

	for n, v := range c.reqCookies {
		cookies = append(cookies, http.Cookie{Name: n, Value: v})
	}

	sort.Slice(cookies, func(i, j int) bool {
		return cookies[i].Name < cookies[j].Name
	})

	for _, v := range cookies {
		v := v
		req.AddCookie(&v)
	}
}

func (c *Client) doOnce() (*http.Request, *http.Response, error) {
	uri, err := c.buildURI()
	if err != nil {
		return nil, nil, err
	}

	body := c.buildBody()

	req, err := http.NewRequestWithContext(c.ctx, c.reqMethod, uri, body)
	if err != nil {
		return nil, nil, err
	}

	c.applyHeaders(req)
	c.applyCookies(req)

	tr := c.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}

	if c.followRedirects {
		cl := http.Client{}
		j, _ := cookiejar.New(nil) //nolint:errcheck // Error is always nil.
		cl.Transport = tr
		cl.Jar = j

		resp, err := cl.Do(req)

		return req, resp, err
	}

	resp, err := tr.RoundTrip(req)

	return req, resp, err
}

// ExpectResponseStatus sets expected response status code.
func (c *Client) ExpectResponseStatus(statusCode int) error {
	return c.expectResp(func() error {
		return c.assertResponseCode(statusCode, c.resp)
	})
}

// ExpectResponseHeader asserts expected response header value.
func (c *Client) ExpectResponseHeader(key, value string) error {
	return c.expectResp(func() error {
		return c.assertResponseHeader(key, value, c.resp)
	})
}

// CheckUnexpectedOtherResponses fails if other responses were present, but not expected with
// ExpectOther* functions.
//
// Does not affect single (non-concurrent) calls.
func (c *Client) CheckUnexpectedOtherResponses() error {
	if c.otherRespExpected || c.otherResp == nil {
		return nil
	}

	return c.assertResponseCode(c.resp.StatusCode, c.otherResp)
}

// ExpectNoOtherResponses sets expectation for only one response status to be received  during concurrent
// calling.
//
// Does not affect single (non-concurrent) calls.
func (c *Client) ExpectNoOtherResponses() error {
	return c.expectResp(func() error {
		if c.otherResp != nil {
			return c.assertResponseCode(c.resp.StatusCode, c.otherResp)
		}

		return nil
	})
}

// ExpectOtherResponsesStatus sets expectation for response status to be received one or more times during concurrent
// calling.
//
// For example, it may describe "Not Found" response on multiple DELETE or "Conflict" response on multiple POST.
// Does not affect single (non-concurrent) calls.
func (c *Client) ExpectOtherResponsesStatus(statusCode int) error {
	c.otherRespExpected = true

	return c.expectResp(func() error {
		if c.otherResp == nil {
			return errNoOtherResponses
		}

		return c.assertResponseCode(statusCode, c.otherResp)
	})
}

// ExpectOtherResponsesHeader sets expectation for response header value to be received one or more times during
// concurrent calling.
func (c *Client) ExpectOtherResponsesHeader(key, value string) error {
	c.otherRespExpected = true

	return c.expectResp(func() error {
		if c.otherResp == nil {
			return errNoOtherResponses
		}

		return c.assertResponseHeader(key, value, c.otherResp)
	})
}

func (c *Client) assertResponseCode(statusCode int, resp *http.Response) error {
	if resp.StatusCode != statusCode {
		return fmt.Errorf("%w, expected: %d (%s), received: %d (%s)", errUnexpectedResponseStatus,
			statusCode, http.StatusText(statusCode), resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	return nil
}

func (c *Client) assertResponseHeader(key, value string, resp *http.Response) error {
	expected, err := json.Marshal(value)
	if err != nil {
		return err
	}

	received, err := json.Marshal(resp.Header.Get(key))
	if err != nil {
		return err
	}

	return c.JSONComparer.FailNotEqual(expected, received)
}

// ExpectResponseBodyCallback sets expectation for response body to be received as JSON payload.
//
// In concurrent mode such response must be met only once or for all calls.
func (c *Client) ExpectResponseBodyCallback(cb func(received []byte) error) error {
	return c.expectResp(func() error {
		return c.checkBody(nil, c.respBody, cb)
	})
}

// ExpectOtherResponsesBodyCallback sets expectation for response body to be received one or more times during concurrent
// calling.
//
// For example, it may describe "Not Found" response on multiple DELETE or "Conflict" response on multiple POST.
// Does not affect single (non-concurrent) calls.
func (c *Client) ExpectOtherResponsesBodyCallback(cb func(received []byte) error) error {
	c.otherRespExpected = true

	return c.expectResp(func() error {
		if c.otherResp == nil {
			return errNoOtherResponses
		}

		return c.checkBody(nil, c.otherRespBody, cb)
	})
}

// ExpectResponseBody sets expectation for response body to be received.
//
// In concurrent mode such response must be met only once or for all calls.
func (c *Client) ExpectResponseBody(body []byte) error {
	return c.expectResp(func() error {
		return c.checkBody(body, c.respBody, nil)
	})
}

// ExpectOtherResponsesBody sets expectation for response body to be received one or more times during concurrent
// calling.
//
// For example, it may describe "Not Found" response on multiple DELETE or "Conflict" response on multiple POST.
// Does not affect single (non-concurrent) calls.
func (c *Client) ExpectOtherResponsesBody(body []byte) error {
	c.otherRespExpected = true

	return c.expectResp(func() error {
		if c.otherResp == nil {
			return errNoOtherResponses
		}

		return c.checkBody(body, c.otherRespBody, nil)
	})
}

func (c *Client) checkBody(expected, received []byte, cb func(received []byte) error) (err error) {
	if len(received) == 0 {
		if len(expected) == 0 {
			return nil
		}

		return errEmptyBody
	}

	defer func() {
		if err != nil && c.OnBodyMismatch != nil {
			c.OnBodyMismatch(received)
		}
	}()

	if (expected == nil || json5.Valid(expected)) && json5.Valid(received) {
		return c.checkJSONBody(expected, received, cb)
	}

	if cb != nil {
		return cb(received)
	}

	if !bytes.Equal(expected, received) {
		return fmt.Errorf("%w, expected: %q, received: %q",
			errUnexpectedBody, string(expected), string(received))
	}

	return nil
}

func (c *Client) checkJSONBody(expected, received []byte, cb func(received []byte) error) (err error) {
	if cb != nil {
		err = cb(received)
	} else {
		expected, err = json5.Downgrade(expected)
		if err != nil {
			return err
		}

		err = c.JSONComparer.FailNotEqual(expected, received)
	}

	if err != nil {
		recCompact, cerr := assertjson.MarshalIndentCompact(json.RawMessage(received), "", " ", 100)
		if cerr == nil {
			received = recCompact
		}

		return fmt.Errorf("%w\nreceived:\n%s ", err, string(received))
	}

	return nil
}

// Concurrently enables concurrent calls to idempotent endpoint.
func (c *Client) Concurrently() *Client {
	c.reqConcurrency = c.ConcurrencyLevel
	if c.reqConcurrency == 0 {
		c.reqConcurrency = defaultConcurrencyLevel
	}

	return c
}
