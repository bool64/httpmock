package httpmock_test

import (
	"context"
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bool64/httpmock"
	"github.com/bool64/shared"
	"github.com/stretchr/testify/assert"
)

func TestNewClient(t *testing.T) {
	cnt := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/foo?q=1", r.URL.String())
		b, err := ioutil.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, `{"foo":"bar"}`, string(b))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "abc", r.Header.Get("X-Header"))
		assert.Equal(t, "def", r.Header.Get("X-Custom"))

		c, err := r.Cookie("c1")
		assert.NoError(t, err)
		assert.Equal(t, "1", c.Value)

		c, err = r.Cookie("c2")
		assert.NoError(t, err)
		assert.Equal(t, "2", c.Value)

		c, err = r.Cookie("foo")
		assert.NoError(t, err)
		assert.Equal(t, "bar", c.Value)

		ncnt := atomic.AddInt64(&cnt, 1)
		rw.Header().Set("Content-Type", "application/json")
		if ncnt > 1 {
			rw.WriteHeader(http.StatusConflict)
			_, err := rw.Write([]byte(`{"error":"conflict"}`))
			assert.NoError(t, err)
		} else {
			rw.WriteHeader(http.StatusAccepted)
			_, err := rw.Write([]byte(`{"bar":"foo", "dyn": "abc"}`))
			assert.NoError(t, err)
		}
	}))

	defer srv.Close()

	vars := &shared.Vars{}

	c := httpmock.NewClient(srv.URL)
	c.JSONComparer.Vars = vars
	c.ConcurrencyLevel = 50
	c.Headers = map[string]string{
		"X-Header": "abc",
	}
	c.Cookies = map[string]string{
		"foo": "bar",
		"c1":  "to-be-overridden",
	}

	c.Reset().
		WithMethod(http.MethodPost).
		WithHeader("X-Custom", "def").
		WithContentType("application/json").
		WithBody([]byte(`{"foo":"bar"}`)).
		WithCookie("c1", "1").
		WithCookie("c2", "2").
		WithURI("/foo?q=1").
		Concurrently()

	assert.NoError(t, c.ExpectResponseStatus(http.StatusAccepted))
	assert.NoError(t, c.ExpectResponseBody([]byte(`{"bar":"foo","dyn":"$var1"}`)))
	assert.NoError(t, c.ExpectResponseBodyCallback(func(received []byte) error {
		return c.JSONComparer.FailMismatch([]byte(`{"bar":"foo"}`), received)
	}))
	assert.Error(t, c.ExpectResponseBodyCallback(func(received []byte) error {
		return c.JSONComparer.FailMismatch([]byte(`{"bar":"foo2"}`), received)
	}))
	assert.NoError(t, c.ExpectResponseHeader("Content-Type", "application/json"))
	assert.NoError(t, c.ExpectOtherResponsesStatus(http.StatusConflict))
	assert.NoError(t, c.ExpectOtherResponsesBody([]byte(`{"error":"conflict"}`)))
	assert.NoError(t, c.ExpectOtherResponsesBodyCallback(func(received []byte) error {
		return c.JSONComparer.FailMismatch([]byte(`{"error":"conflict"}`), received)
	}))
	assert.Error(t, c.ExpectOtherResponsesBodyCallback(func(received []byte) error {
		return c.JSONComparer.FailMismatch([]byte(`{"error":"conflict2"}`), received)
	}))
	assert.NoError(t, c.ExpectOtherResponsesHeader("Content-Type", "application/json"))
	assert.NoError(t, c.CheckUnexpectedOtherResponses())
	assert.EqualError(t, c.ExpectNoOtherResponses(), "unexpected response status, expected: 202 (Accepted), received: 409 (Conflict)")

	val, found := vars.Get("$var1")
	assert.True(t, found)
	assert.Equal(t, "abc", val)
}

func TestNewClient_failedExpectation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, err := writer.Write([]byte(`{"bar":"foo"}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()
	c := httpmock.NewClient(srv.URL)

	c.OnBodyMismatch = func(received []byte) {
		assert.Equal(t, `{"bar":"foo"}`, string(received))
		println(received)
	}

	c.WithURI("/")
	assert.EqualError(t, c.ExpectResponseBody([]byte(`{"foo":"bar}"`)),
		"unexpected body, expected: \"{\\\"foo\\\":\\\"bar}\\\"\", received: \"{\\\"bar\\\":\\\"foo\\\"}\"")
}

func TestNewClient_followRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.RequestURI == "/one" {
			http.Redirect(writer, request, srv.URL+"/two", http.StatusFound)

			return
		}

		if request.RequestURI == "/two" {
			http.Redirect(writer, request, srv.URL+"/three", http.StatusMovedPermanently)

			return
		}

		_, err := writer.Write([]byte(`{"bar":"foo"}`))
		assert.NoError(t, err)
	}))

	defer srv.Close()

	c := httpmock.NewClient(srv.URL)
	c.FollowRedirects()

	c.WithURI("/one")

	assert.NoError(t, c.ExpectResponseStatus(http.StatusOK))
	assert.NoError(t, c.ExpectResponseBody([]byte(`{"bar":"foo"}`)))
}

func TestNewClient_context(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, err := writer.Write([]byte(`{"bar":"foo"}`))
		assert.NoError(t, err)
	}))

	defer srv.Close()

	c := httpmock.NewClient(srv.URL)
	c.FollowRedirects()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.WithURI("/one")
	c.WithContext(ctx)

	assert.True(t, errors.Is(c.ExpectResponseStatus(http.StatusOK), context.Canceled))
}

func TestNewClient_formData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/one?foo=bar&foo=baz&qux=quux", request.RequestURI)
		assert.NoError(t, request.ParseForm())
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "foo=bar&foo=baz&qux=quux", request.PostForm.Encode())

		_, err := writer.Write([]byte(`{"bar":"foo"}`))
		assert.NoError(t, err)
	}))

	defer srv.Close()

	c := httpmock.NewClient(srv.URL)

	c.WithURI("/one?foo=bar")
	c.WithQueryParam("foo", "baz")
	c.WithQueryParam("qux", "quux")
	c.WithURLEncodedFormDataParam("foo", "bar")
	c.WithURLEncodedFormDataParam("foo", "baz")
	c.WithURLEncodedFormDataParam("qux", "quux")

	assert.EqualError(t, c.ExpectResponseBody([]byte(`{"foo":"bar}"`)),
		"unexpected body, expected: \"{\\\"foo\\\":\\\"bar}\\\"\", received: \"{\\\"bar\\\":\\\"foo\\\"}\"")
}

func TestClient_Fork(t *testing.T) {
	c1 := httpmock.NewClient("https://one")
	c2 := httpmock.NewClient("https://two")

	c1.JSONComparer.Vars = &shared.Vars{}
	c1.JSONComparer.Vars.Set("foo", 123)

	wg := sync.WaitGroup{}
	for i := 0; i < 100; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ctx := context.Background()

			ctx, fc1 := c1.Fork(ctx)
			assert.NotNil(t, fc1)
			fc1.WithMethod(http.MethodGet)

			fc1.JSONComparer.Vars.Set("bar", 345)
			foo, ok := fc1.JSONComparer.Vars.Get("foo")
			assert.True(t, ok)
			assert.Equal(t, 123, foo)

			ctx, fc2 := c2.Fork(ctx)
			assert.NotNil(t, fc2)
			fc2.WithMethod(http.MethodPost)

			ctx1, fc1a := c1.Fork(ctx)
			assert.Equal(t, fc1a, fc1)
			assert.True(t, c1 != fc1)
			assert.Equal(t, ctx, ctx1)

			ctx2, fc2a := c2.Fork(ctx)
			assert.Equal(t, fc2a, fc2)
			assert.True(t, c2 != fc2)
			assert.Equal(t, ctx, ctx2)
		}()
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-time.After(10 * time.Second):
		assert.Fail(t, "could not wait for goroutines to finish")
	case <-done:
	}
}

func TestClient_AllowRetries(t *testing.T) {
	tries := 0

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		tries++

		if tries == 5 {
			writer.WriteHeader(http.StatusOK)

			return
		}

		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := httpmock.NewClient(srv.URL)

	c.WithMethod(http.MethodGet)
	c.WithURI("/")

	retriesLeft := 10

	c.AllowRetries(httpmock.RetryBackOffFunc(func() time.Duration {
		retriesLeft--

		if retriesLeft <= 0 {
			return -1
		}

		return time.Millisecond
	}))

	assert.NoError(t, c.ExpectResponseStatus(http.StatusOK))
	assert.Equal(t, 5, tries)
}
