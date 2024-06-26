package httpmock_test

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bool64/httpmock"
	"github.com/bool64/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertRoundTrip(t *testing.T, baseURL string, expectation httpmock.Expectation) {
	t.Helper()

	var bodyReader io.Reader

	if expectation.RequestBody != nil {
		bodyReader = bytes.NewReader(expectation.RequestBody)
	}

	req, err := http.NewRequest(expectation.Method, baseURL+expectation.RequestURI, bodyReader)
	require.NoError(t, err)

	for k, v := range expectation.RequestHeader {
		req.Header.Set(k, v)
	}

	for n, v := range expectation.RequestCookie {
		req.AddCookie(&http.Cookie{Name: n, Value: v})
	}

	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)

	body, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)

	if expectation.Status == 0 {
		expectation.Status = http.StatusOK
	}

	assert.Equal(t, expectation.Status, resp.StatusCode)
	assert.Equal(t, string(expectation.ResponseBody), string(body))

	// Asserting default for successful responses.
	if resp.StatusCode != http.StatusInternalServerError {
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	}

	if len(expectation.ResponseHeader) > 0 {
		for k, v := range expectation.ResponseHeader {
			assert.Equal(t, v, resp.Header.Get(k))
		}
	}
}

func TestServer_ServeHTTP(t *testing.T) {
	// Creating REST service mock.
	mock, baseURL := httpmock.NewServer()
	defer mock.Close()

	mock.OnBodyMismatch = func(received []byte) {
		assert.Equal(t, `{"foo":"bar"}`, string(received))
	}

	mock.DefaultResponseHeaders = map[string]string{
		"Content-Type": "application/json",
	}

	// Requesting mock without expectations fails.
	assertRoundTrip(t, baseURL, httpmock.Expectation{
		RequestURI:   "/test?test=test",
		Status:       http.StatusInternalServerError,
		ResponseBody: []byte("unexpected request received: GET /test?test=test"),
	})

	// Requesting mock without expectations fails.
	assertRoundTrip(t, baseURL, httpmock.Expectation{
		RequestURI:   "/test?test=test",
		Status:       http.StatusInternalServerError,
		RequestBody:  []byte(`{"foo":"bar"}`),
		ResponseBody: []byte("unexpected request received: GET /test?test=test, body:\n{\"foo\":\"bar\"}"),
	})

	// Setting expectations for first request.
	exp1 := httpmock.Expectation{
		Method:        http.MethodPost,
		RequestURI:    "/test?test=test",
		RequestHeader: map[string]string{"Authorization": "Bearer token"},
		RequestCookie: map[string]string{"c1": "v1", "c2": "v2"},
		RequestBody:   []byte(`{"request":"body"}`),

		Status:       http.StatusCreated,
		ResponseBody: []byte(`{"response":"body"}`),
	}
	mock.Expect(exp1)

	// Setting expectations for second request.
	exp2 := httpmock.Expectation{
		Method:      http.MethodPost,
		RequestURI:  "/test?test=test",
		RequestBody: []byte(`not a JSON`),

		ResponseHeader: map[string]string{
			"X-Foo": "bar",
		},
		ResponseBody: []byte(`{"response":"body2"}`),
	}
	mock.Expect(exp2)

	// Sending first request.
	assertRoundTrip(t, baseURL, exp1)

	// Expectations were not met yet.
	require.EqualError(t, mock.ExpectationsWereMet(),
		"there are remaining expectations that were not met: POST /test?test=test")

	// Sending second request.
	assertRoundTrip(t, baseURL, exp2)

	// Expectations were met.
	require.NoError(t, mock.ExpectationsWereMet())

	// Requesting mock without expectations fails.
	assertRoundTrip(t, baseURL, httpmock.Expectation{
		RequestURI:   "/test?test=test",
		Status:       http.StatusInternalServerError,
		ResponseBody: []byte("unexpected request received: GET /test?test=test"),
	})
}

func TestServer_ServeHTTP_error(t *testing.T) {
	// Creating REST service mock.
	mock, baseURL := httpmock.NewServer()
	defer mock.Close()

	mock.OnBodyMismatch = func(received []byte) {
		assert.Equal(t, `{"request":"body"}`, string(received))
	}

	// Setting expectations for first request.
	mock.Expect(httpmock.Expectation{
		Method:        http.MethodPost,
		RequestURI:    "/test?test=test",
		RequestHeader: map[string]string{"X-Foo": "bar"},
		RequestBody:   []byte(`{"foo":"bar"}`),
	})

	// Sending request with wrong uri.
	req, err := http.NewRequest(http.MethodPost, baseURL+"/wrong-uri", bytes.NewReader([]byte(`{"request":"body"}`)))
	require.NoError(t, err)
	req.Header.Set("X-Foo", "bar")

	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)

	respBody, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, `request uri "/test?test=test" expected, "/wrong-uri" received`, string(respBody))

	// Sending request with wrong method.
	req, err = http.NewRequest(http.MethodGet, baseURL+"/test?test=test", bytes.NewReader([]byte(`{"request":"body"}`)))
	require.NoError(t, err)
	req.Header.Set("X-Foo", "bar")

	resp, err = http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)

	respBody, err = ioutil.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, `method "POST" expected, "GET" received`, string(respBody))

	// Sending request with wrong header.
	req, err = http.NewRequest(http.MethodPost, baseURL+"/test?test=test", bytes.NewReader([]byte(`{"request":"body"}`)))
	require.NoError(t, err)
	req.Header.Set("X-Foo", "space")

	resp, err = http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)

	respBody, err = ioutil.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, `header "X-Foo" with value "bar" expected, "space" received`, string(respBody))

	// Sending request with wrong body.
	req, err = http.NewRequest(http.MethodPost, baseURL+"/test?test=test", bytes.NewReader([]byte(`{"request":"body"}`)))
	require.NoError(t, err)
	req.Header.Set("X-Foo", "bar")

	resp, err = http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)

	respBody, err = ioutil.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, `unexpected request body: not equal:
 {
-  "foo": "bar"
+  "request": "body"
 }
`, string(respBody))
}

func TestServer_ServeHTTP_concurrency(t *testing.T) {
	// Creating REST service mock.
	mock, url := httpmock.NewServer()
	defer mock.Close()

	n := 50

	for i := 0; i < n; i++ {
		// Setting expectations for first request.
		mock.Expect(httpmock.Expectation{
			Method:       http.MethodGet,
			RequestURI:   "/test?test=test",
			ResponseBody: []byte("body"),
		})
	}

	wg := sync.WaitGroup{}
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()

			// Sending request with wrong header.
			req, err := http.NewRequest(http.MethodGet, url+"/test?test=test", nil)
			assert.NoError(t, err)
			req.Header.Set("X-Foo", "space")

			resp, err := http.DefaultTransport.RoundTrip(req)
			assert.NoError(t, err)

			respBody, err := ioutil.ReadAll(resp.Body)
			assert.NoError(t, resp.Body.Close())
			assert.NoError(t, err)

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, `body`, string(respBody))
		}()
	}

	wg.Wait()
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestServer_ResetExpectations(t *testing.T) {
	// Creating REST service mock.
	mock, _ := httpmock.NewServer()
	defer mock.Close()

	mock.Expect(httpmock.Expectation{
		Method:       http.MethodGet,
		RequestURI:   "/test?test=test",
		ResponseBody: []byte("body"),
	})

	mock.ExpectAsync(httpmock.Expectation{
		Method:       http.MethodGet,
		RequestURI:   "/test-async?test=test",
		ResponseBody: []byte("body"),
	})

	require.Error(t, mock.ExpectationsWereMet())
	mock.ResetExpectations()
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestServer_vars(t *testing.T) {
	sm, url := httpmock.NewServer()
	sm.JSONComparer.Vars = &shared.Vars{}
	sm.Expect(httpmock.Expectation{
		Method:       http.MethodGet,
		RequestURI:   "/",
		RequestBody:  []byte(`{"foo":"bar","dyn":"$var1"}`),
		ResponseBody: []byte(`{"bar":"foo","dynEcho":"$var1"}`),
	})

	req, err := http.NewRequest(http.MethodGet, url+"/", strings.NewReader(`{"foo":"bar","dyn":"abc"}`))
	require.NoError(t, err)

	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)

	body, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, err)

	require.NoError(t, resp.Body.Close())

	assert.Equal(t, `{"bar":"foo","dynEcho":"abc"}`, string(body))
}

func TestServer_ExpectAsync(t *testing.T) {
	sm, url := httpmock.NewServer()
	sm.Expect(httpmock.Expectation{
		Method:       http.MethodGet,
		RequestURI:   "/",
		ResponseBody: []byte(`{"bar":"foo"}`),
	})
	sm.ExpectAsync(httpmock.Expectation{
		Method:       http.MethodGet,
		RequestURI:   "/async1",
		ResponseBody: []byte(`{"bar":"async1"}`),
	})
	sm.ExpectAsync(httpmock.Expectation{
		Method:       http.MethodGet,
		RequestURI:   "/async2",
		ResponseBody: []byte(`{"bar":"async2"}`),
		Unlimited:    true,
	})

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()

		req, err := http.NewRequest(http.MethodGet, url+"/async1", nil)
		assert.NoError(t, err)

		resp, err := http.DefaultTransport.RoundTrip(req)
		assert.NoError(t, err)

		body, err := ioutil.ReadAll(resp.Body)
		assert.NoError(t, err)

		assert.NoError(t, resp.Body.Close())
		assert.Equal(t, `{"bar":"async1"}`, string(body))
	}()

	go func() {
		defer wg.Done()

		for i := 0; i < 50; i++ {
			req, err := http.NewRequest(http.MethodGet, url+"/async2", nil)
			assert.NoError(t, err)

			resp, err := http.DefaultTransport.RoundTrip(req)
			assert.NoError(t, err)

			body, err := ioutil.ReadAll(resp.Body)
			assert.NoError(t, err)

			assert.NoError(t, resp.Body.Close())
			assert.Equal(t, `{"bar":"async2"}`, string(body))
		}
	}()

	req, err := http.NewRequest(http.MethodGet, url+"/", nil)
	require.NoError(t, err)

	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)

	body, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, err)

	require.NoError(t, resp.Body.Close())
	assert.Equal(t, `{"bar":"foo"}`, string(body))

	wg.Wait()
	require.NoError(t, sm.ExpectationsWereMet())
}
