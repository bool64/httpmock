package httpmock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/swaggest/assertjson"
	"github.com/swaggest/assertjson/json5"
)

// Expectation describes expected request and defines response.
type Expectation struct {
	Method        string
	RequestURI    string
	RequestHeader map[string]string
	RequestCookie map[string]string
	RequestBody   []byte

	Status         int
	ResponseHeader map[string]string
	ResponseBody   []byte

	// Unlimited enables reusing of this expectation unlimited number of times.
	Unlimited bool
	// Repeated defines how many times this expectation should be used.
	Repeated int
}

// Server serves predefined response for predefined request.
type Server struct {
	// OnError is called on expectations mismatch or internal errors, optional.
	OnError func(err error)

	// OnRequest is called on before every request, optional.
	OnRequest func(rw http.ResponseWriter, req *http.Request)

	// ErrorResponder allows custom failure responses.
	ErrorResponder func(rw http.ResponseWriter, err error)

	// DefaultResponseHeaders are added to every response to an expected request.
	DefaultResponseHeaders map[string]string

	// JSONComparer controls JSON equality check.
	JSONComparer assertjson.Comparer

	// OnBodyMismatch is called when received body does not match expected, optional.
	OnBodyMismatch func(received []byte)

	mu           sync.Mutex
	server       *httptest.Server
	expectations []Expectation
	async        []Expectation
}

// NewServer creates mocked server.
func NewServer() (*Server, string) {
	m := Server{}
	m.server = httptest.NewServer(&m)
	m.JSONComparer = assertjson.Comparer{IgnoreDiff: assertjson.IgnoreDiff}

	return &m, m.server.URL
}

// Expect adds expected operation.
func (sm *Server) Expect(e Expectation) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.expectations = append(sm.expectations, e)
}

// ExpectAsync sets non-sequential expectation.
//
// Asynchronous expectations are checked for every incoming request,
// first match is used for response.
// If there are no matches, regular (sequential expectations are used).
func (sm *Server) ExpectAsync(e Expectation) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.async = append(sm.async, e)
}

// Close closes mocked server.
func (sm *Server) Close() {
	sm.server.Close()
}

func (sm *Server) prepareBody(data []byte) ([]byte, error) {
	if sm.JSONComparer.Vars == nil {
		return data, nil
	}

	if !json.Valid(data) {
		return data, nil
	}

	for k, v := range sm.JSONComparer.Vars.GetAll() {
		j, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}

		data = bytes.ReplaceAll(data, []byte(`"`+k+`"`), j)
	}

	return data, nil
}

func (sm *Server) writeResponse(rw http.ResponseWriter, expectation Expectation) bool {
	var err error

	if sm.DefaultResponseHeaders != nil {
		for k, v := range sm.DefaultResponseHeaders {
			rw.Header().Set(k, v)
		}
	}

	if expectation.ResponseHeader != nil {
		for k, v := range expectation.ResponseHeader {
			rw.Header().Set(k, v)
		}
	}

	if expectation.Status == 0 {
		expectation.Status = http.StatusOK
	}

	rw.WriteHeader(expectation.Status)

	expectation.ResponseBody, err = sm.prepareBody(expectation.ResponseBody)
	if sm.checkFail(rw, err) {
		return false
	}

	_, err = rw.Write(expectation.ResponseBody)

	return !sm.checkFail(rw, err)
}

func (sm *Server) checkAsync(rw http.ResponseWriter, req *http.Request) bool {
	for i, expectation := range sm.async {
		if err := sm.checkRequest(req, expectation); err != nil {
			continue
		}

		if !sm.writeResponse(rw, expectation) {
			return true
		}

		if expectation.Unlimited {
			return true
		}

		if expectation.Repeated > 0 {
			expectation.Repeated--
			sm.async[i] = expectation

			if expectation.Repeated > 0 {
				return true
			}
		}

		// Deleting expectation.
		sm.async[i] = sm.async[len(sm.async)-1]
		sm.async = sm.async[:len(sm.async)-1]

		return true
	}

	return false
}

// ServeHTTP asserts request expectations and serves mocked response.
func (sm *Server) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.OnRequest != nil {
		sm.OnRequest(rw, req)
	}

	if sm.checkAsync(rw, req) {
		return
	}

	if len(sm.expectations) == 0 {
		body, err := ioutil.ReadAll(req.Body)
		if err == nil && len(body) > 0 {
			if sm.OnBodyMismatch != nil {
				sm.OnBodyMismatch(body)
			}

			sm.checkFail(rw, fmt.Errorf("unexpected request received: %s %s, body:\n%s", req.Method,
				req.RequestURI, string(body)))
		} else {
			sm.checkFail(rw, fmt.Errorf("unexpected request received: %s %s", req.Method, req.RequestURI))
		}

		return
	}

	expectation := sm.expectations[0]

	err := sm.checkRequest(req, expectation)
	if sm.checkFail(rw, err) {
		return
	}

	if !sm.writeResponse(rw, expectation) {
		return
	}

	if expectation.Unlimited {
		return
	}

	if expectation.Repeated > 0 {
		expectation.Repeated--
		sm.expectations[0] = expectation

		if expectation.Repeated > 0 {
			return
		}
	}

	sm.expectations = sm.expectations[1:]
}

func (sm *Server) checkBody(expected, received []byte) (err error) {
	defer func() {
		if err != nil && sm.OnBodyMismatch != nil {
			sm.OnBodyMismatch(received)
		}
	}()

	if !json5.Valid(expected) || !json5.Valid(received) {
		if !bytes.Equal(expected, received) {
			return errors.New("unexpected request body")
		}
	} else {
		// Performing JSON comparison for JSON payloads and binary comparison otherwise.
		expected, err := json5.Downgrade(expected)
		if err != nil {
			return err
		}

		if err = sm.JSONComparer.FailNotEqual(expected, received); err != nil {
			return fmt.Errorf("unexpected request body: %w", err)
		}
	}

	return nil
}

func (sm *Server) checkRequest(req *http.Request, expectation Expectation) error {
	if expectation.Method != "" && expectation.Method != req.Method {
		return fmt.Errorf("method %q expected, %q received", expectation.Method, req.Method)
	}

	if expectation.RequestURI != "" && expectation.RequestURI != req.RequestURI {
		return fmt.Errorf("request uri %q expected, %q received", expectation.RequestURI, req.RequestURI)
	}

	for k, v := range expectation.RequestHeader {
		if req.Header.Get(k) != v {
			return fmt.Errorf("header %q with value %q expected, %q received", k, v, req.Header.Get(k))
		}
	}

	for n, v := range expectation.RequestCookie {
		c, err := req.Cookie(n)
		if err != nil {
			return fmt.Errorf("failed to find cookie %q with value %q: %w", n, v, err)
		}

		if c.Value != v {
			return fmt.Errorf("header %q with value %q expected, %q received", n, v, c.Value)
		}
	}

	reqBody, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return err
	}

	if expectation.RequestBody == nil {
		return nil
	}

	return sm.checkBody(expectation.RequestBody, reqBody)
}

func (sm *Server) checkFail(rw http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}

	if sm.OnError != nil {
		sm.OnError(err)
	}

	if sm.ErrorResponder != nil {
		sm.ErrorResponder(rw, err)

		return true
	}

	rw.WriteHeader(http.StatusInternalServerError)

	_, err = rw.Write([]byte(err.Error()))
	if err != nil && sm.OnError != nil {
		sm.OnError(err)
	}

	return true
}

// ResetExpectations discards all expectation to reset the state of mock.
func (sm *Server) ResetExpectations() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.expectations = nil
	sm.async = nil
}

// ExpectationsWereMet checks whether all queued expectations
// were met in order. If any of them was not met - an error is returned.
func (sm *Server) ExpectationsWereMet() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	var unmet string

	for _, e := range sm.expectations {
		if e.Unlimited {
			continue
		}

		if e.Method != "" || e.RequestURI != "" {
			unmet += ", " + e.Method + " " + e.RequestURI
		} else {
			unmet += ", response " + string(e.ResponseBody)
		}
	}

	for _, e := range sm.async {
		if e.Unlimited {
			continue
		}

		if e.Method != "" || e.RequestURI != "" {
			unmet += ", " + e.Method + " " + e.RequestURI
		} else {
			unmet += ", response " + string(e.ResponseBody)
		}
	}

	if unmet != "" {
		return errors.New("there are remaining expectations that were not met: " + unmet[2:])
	}

	return nil
}
