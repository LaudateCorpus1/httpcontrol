// Package httpcontrol allows a HTTP transport supporting connection pooling,
// timeouts & retries.
//
// This Transport is built on top of the standard library transport and
// augments it with additional features.
package httpcontrol

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Stats for a RoundTrip.
type Stats struct {
	// The RoundTrip request.
	Request *http.Request

	// May not always be available.
	Response *http.Response

	// Will be set if the RoundTrip resulted in an error. Note that these are
	// RoundTrip errors and we do not care about the HTTP Status.
	Error error

	// Each duration is independent and the sum of all of them is the total
	// request duration. One or more durations may be zero.
	Duration struct {
		Header, Body time.Duration
	}

	Retry struct {
		// Will be incremented for each retry. The initial request will have this
		// set to 0, and the first retry to 1 and so on.
		Count uint

		// Will be set if and only if an error was encountered and a retry is
		// pending.
		Pending bool
	}
}

// A human readable representation often useful for debugging.
func (s *Stats) String() string {
	var b strings.Builder

	b.WriteString(s.Request.Method)
	b.WriteString(" ")
	b.WriteString(s.Request.URL.String())

	if s.Response != nil {
		b.WriteString(" got response with status ")
		b.WriteString(s.Response.Status)
	}

	return b.String()
}

// Transport is an implementation of RoundTripper that supports http, https,
// and http proxies (for either http or https with CONNECT). Transport can
// cache connections for future re-use, provides various timeouts, retry logic
// and the ability to track request statistics.
type Transport struct {
	http.Transport

	// DialTimeout is the maximum amount of time a dial will wait for
	// a connect to complete. If Deadline is also set, it may fail
	// earlier.
	//
	// The default is no timeout.
	//
	// When using TCP and dialing a host name with multiple IP
	// addresses, the timeout may be divided between them.
	//
	// With or without a timeout, the operating system may impose
	// its own earlier timeout. For instance, TCP timeouts are
	// often around 3 minutes.
	DialTimeout time.Duration

	// DialKeepAlive specifies the interval between keep-alive
	// probes for an active network connection.
	// If zero, keep-alive probes are sent with a default value
	// (currently 15 seconds), if supported by the protocol and operating
	// system. Network protocols or operating systems that do
	// not support keep-alives ignore this field.
	// If negative, keep-alive probes are disabled.
	DialKeepAlive time.Duration

	// RequestTimeout, if non-zero, specifies the amount of time for the entire
	// request. This includes dialing (if necessary), the response header as well
	// as the entire body.
	//
	// Deprecated: Use Request.WithContext to create a request with a
	// cancelable context instead. RequestTimeout cannot cancel HTTP/2
	// requests.
	RequestTimeout time.Duration

	// RetryAfterTimeout, if true, will enable retries for a number of failures
	// that are probably safe to retry for most cases but, depending on the
	// context, might not be safe. Retried errors: net.Errors where Timeout()
	// returns `true` or timeouts that bubble up as url.Error but were originally
	// net.Error, OpErrors where the request was cancelled (either by this lib or
	// by the calling code, or finally errors from requests that were cancelled
	// before the remote side was contacted.
	RetryAfterTimeout bool

	// MaxTries, if non-zero, specifies the number of times we will retry on
	// failure. Retries are only attempted for temporary network errors or known
	// safe failures.
	MaxTries uint

	// Stats allows for capturing the result of a request and is useful for
	// monitoring purposes.
	Stats func(*Stats)

	startOnce sync.Once

	requestCancelMap   map[*http.Request]context.CancelFunc
	requestCancelMutex sync.Mutex
}

var knownFailureSuffixes = []string{
	unix.ECONNREFUSED.Error(),
	unix.ECONNRESET.Error(),
	unix.ETIMEDOUT.Error(),
	"no such host",
	"remote error: handshake failure",
	io.ErrUnexpectedEOF.Error(),
	io.EOF.Error(),
}

func (t *Transport) shouldRetryError(err error) bool {
	if neterr, ok := err.(net.Error); ok {
		if neterr.Temporary() {
			return true
		}
	}

	if t.RetryAfterTimeout {
		if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
			return true
		}

		// http://stackoverflow.com/questions/23494950/specifically-check-for-timeout-error
		if urlerr, ok := err.(*url.Error); ok {
			var neturlerr net.Error
			if neturlerr, ok = urlerr.Err.(net.Error); ok && neturlerr.Timeout() {
				return true
			}
		}
		if operr, ok := err.(*net.OpError); ok {
			if strings.Contains(operr.Error(), "use of closed network connection") {
				return true
			}
		}

		// The request timed out before we could connect
		if strings.Contains(err.Error(), "request canceled while waiting for connection") {
			return true
		}
	}

	s := err.Error()
	for _, suffix := range knownFailureSuffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// Start the Transport.
func (t *Transport) start() {
	if (t.DialContext == nil) && (t.Dial == nil) {
		if (t.DialTimeout != 0) || (t.DialKeepAlive != 0) {
			dialer := &net.Dialer{
				Timeout:   t.DialTimeout,
				KeepAlive: t.DialKeepAlive,
			}
			t.DialContext = dialer.DialContext
		}
	}

	if t.requestCancelMap == nil {
		t.requestCancelMap = make(map[*http.Request]context.CancelFunc)
	}
}

// CloseIdleConnections closes the idle connections.
func (t *Transport) CloseIdleConnections() {
	t.startOnce.Do(t.start)
	t.Transport.CloseIdleConnections()
}

// CancelRequest cancels an in-flight request by closing its connection.
// CancelRequest should only be called after RoundTrip has returned.
//
// Deprecated: Use Request.WithContext to create a request with a
// cancelable context instead. CancelRequest cannot cancel HTTP/2
// requests.
func (t *Transport) CancelRequest(req *http.Request) {
	t.startOnce.Do(t.start)

	if t.RequestTimeout != 0 {
		t.requestCancelMutex.Lock()
		cancelFunc, ok := t.requestCancelMap[req]
		if ok {
			delete(t.requestCancelMap, req)
		}
		t.requestCancelMutex.Unlock()
		if cancelFunc != nil {
			cancelFunc()
		}
	} else {
		t.Transport.CancelRequest(req)
	}
}

func (t *Transport) tries(req *http.Request, try uint) (*http.Response, error) {
	startTime := time.Now()
	res, err := t.Transport.RoundTrip(req)
	headerTime := time.Now()
	if err != nil {
		var stats *Stats
		if t.Stats != nil {
			stats = &Stats{
				Request:  req,
				Response: res,
				Error:    err,
			}
			stats.Duration.Header = headerTime.Sub(startTime)
			stats.Retry.Count = try
		}

		if try < t.MaxTries && req.Method == http.MethodGet && t.shouldRetryError(err) {
			if stats != nil {
				stats.Retry.Pending = true
				t.Stats(stats)
			}
			return t.tries(req, try+1)
		}

		if t.Stats != nil {
			t.Stats(stats)
		}
		return nil, err
	}

	res.Body = &bodyCloser{
		ReadCloser: res.Body,
		res:        res,
		transport:  t,
		startTime:  startTime,
		headerTime: headerTime,
	}
	return res, nil
}

// RoundTrip implements the RoundTripper interface.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.startOnce.Do(t.start)
	if t.RequestTimeout != 0 {
		origReq := req

		ctx, cancelFunc := context.WithTimeout(req.Context(), t.RequestTimeout)
		t.requestCancelMutex.Lock()
		t.requestCancelMap[req] = cancelFunc
		t.requestCancelMutex.Unlock()

		defer func() {
			t.requestCancelMutex.Lock()
			delete(t.requestCancelMap, origReq)
			t.requestCancelMutex.Unlock()
		}()

		req = req.WithContext(ctx)
	}
	return t.tries(req, 0)
}

type bodyCloser struct {
	io.ReadCloser
	res        *http.Response
	transport  *Transport
	startTime  time.Time
	headerTime time.Time
}

// Close implements the Closer interface
func (b *bodyCloser) Close() error {
	err := b.ReadCloser.Close()
	closeTime := time.Now()
	if b.transport.Stats != nil {
		stats := &Stats{
			Request:  b.res.Request,
			Response: b.res,
		}
		stats.Duration.Header = b.headerTime.Sub(b.startTime)
		stats.Duration.Body = closeTime.Sub(b.startTime) - stats.Duration.Header
		b.transport.Stats(stats)
	}
	return err
}
