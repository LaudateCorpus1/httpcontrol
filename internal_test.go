package httpcontrol

import (
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/onsi/gomega"
)

type mockNetError struct {
	temporary bool
	timeout   bool
}

func (t mockNetError) Error() string   { return "" }
func (t mockNetError) Temporary() bool { return t.temporary }
func (t mockNetError) Timeout() bool   { return t.timeout }

func TestShouldRetry(t *testing.T) {
	gomega.RegisterTestingT(t)

	r := Transport{RetryAfterTimeout: true}
	cases := []error{
		mockNetError{temporary: true},
		mockNetError{timeout: true},
		&url.Error{Err: mockNetError{timeout: true}},
		errors.New("request canceled while waiting for connection"),
		&net.OpError{Err: errors.New("use of closed network connection")},
	}
	for _, s := range knownFailureSuffixes {
		cases = append(cases, errors.New(s))
	}
	for _, err := range cases {
		gomega.Expect(r.shouldRetryError(err)).To(gomega.BeTrue())
	}
}

func TestShouldNotRetryRandomError(t *testing.T) {
	gomega.RegisterTestingT(t)

	var r Transport
	gomega.Expect(r.shouldRetryError(errors.New(""))).To(gomega.BeFalse())
}
