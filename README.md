# httpmock

[![Build Status](https://github.com/bool64/httpmock/workflows/test-unit/badge.svg)](https://github.com/bool64/httpmock/actions?query=branch%3Amaster+workflow%3Atest-unit)
[![Coverage Status](https://codecov.io/gh/bool64/httpmock/branch/master/graph/badge.svg)](https://codecov.io/gh/bool64/httpmock)
[![GoDevDoc](https://img.shields.io/badge/dev-doc-00ADD8?logo=go)](https://pkg.go.dev/github.com/bool64/httpmock)
[![Time Tracker](https://wakatime.com/badge/github/bool64/httpmock.svg)](https://wakatime.com/badge/github/bool64/httpmock)
![Code lines](https://sloc.xyz/github/bool64/httpmock/?category=code)
![Comments](https://sloc.xyz/github/bool64/httpmock/?category=comments)

HTTP Server and Client mocks for Go.

## Example

```go
// Prepare server mock.
sm, url := httpmock.NewServer()
defer sm.Close()

// This example shows Client and Server working together for sake of portability.
// In real-world scenarios Client would complement real server or Server would complement real HTTP client.

// Set successful expectation for first request out of concurrent batch.
exp := httpmock.Expectation{
    Method:     http.MethodPost,
    RequestURI: "/foo?q=1",
    RequestHeader: map[string]string{
        "X-Custom":     "def",
        "X-Header":     "abc",
        "Content-Type": "application/json",
    },
    RequestBody:  []byte(`{"foo":"bar"}`),
    Status:       http.StatusAccepted,
    ResponseBody: []byte(`{"bar":"foo"}`),
}
sm.Expect(exp)

// Set failing expectation for other requests of concurrent batch.
exp.Status = http.StatusConflict
exp.ResponseBody = []byte(`{"error":"conflict"}`)
exp.Unlimited = true
sm.Expect(exp)

// Prepare client request.
c := httpmock.NewClient(url)
c.ConcurrencyLevel = 50
c.Headers = map[string]string{
    "X-Header": "abc",
}

c.Reset().
    WithMethod(http.MethodPost).
    WithHeader("X-Custom", "def").
    WithContentType("application/json").
    WithBody([]byte(`{"foo":"bar"}`)).
    WithURI("/foo?q=1").
    Concurrently()

// Check expectations errors.
fmt.Println(
    c.ExpectResponseStatus(http.StatusAccepted),
    c.ExpectResponseBody([]byte(`{"bar":"foo"}`)),
    c.ExpectOtherResponsesStatus(http.StatusConflict),
    c.ExpectOtherResponsesBody([]byte(`{"error":"conflict"}`)),
)

// Output:
// <nil> <nil> <nil> <nil>
```