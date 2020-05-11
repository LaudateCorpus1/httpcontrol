// Package httpcache provides a cache enabled http Transport.
package httpcache

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"time"
)

// ByteCache is a key-value cache store
type ByteCache interface {
	Store(key string, value []byte, timeout time.Duration) error
	Get(key string) ([]byte, error)
}

// Config provides cache key & timeout logic
type Config interface {
	// Key is a function that generates the cache key for the given http.Request. An empty string will disable caching.
	Key(req *http.Request) string

	// MaxAge provides the max cache age for the given request/response pair. A zero value will disable caching for the
	// pair. The request is available via res.Request.
	MaxAge(res *http.Response) time.Duration
}

// Transport is a cache enabled http.Transport.
type Transport struct {
	Config    Config            // Provides cache key & timeout logic.
	ByteCache ByteCache         // Cache where serialized responses will be stored.
	Transport http.RoundTripper // The underlying http.RoundTripper for actual requests.
}

type cacheEntry struct {
	Response *http.Response
	Body     []byte
}

// RoundTrip implements the RoundTripper interface.
func (t *Transport) RoundTrip(req *http.Request) (res *http.Response, err error) {
	key := t.Config.Key(req)
	var entry cacheEntry

	// from cache
	if key != "" {
		var raw []byte
		raw, err = t.ByteCache.Get(key)
		if err != nil {
			return nil, err
		}

		if raw != nil {
			if err = json.Unmarshal(raw, &entry); err != nil {
				return nil, err
			}

			// setup fake http.Response
			res = entry.Response
			res.Body = ioutil.NopCloser(bytes.NewReader(entry.Body))
			res.Request = req
			return res, nil
		}
	}

	// real request
	res, err = t.Transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// no caching required
	if key == "" {
		return res, nil
	}

	// fully buffer response for caching purposes
	body, err := ioutil.ReadAll(res.Body)
	if closeErr := res.Body.Close(); closeErr != nil {
		log.Printf("error closing body for %q: %v\n", res.Request.URL.String(), closeErr)
	}
	if err != nil {
		return nil, err
	}

	// remove properties we want to skip in serialization
	res.Body = nil
	res.Request = nil

	// serialize the cache entry
	entry.Response = res
	entry.Body = body
	raw, err := json.Marshal(&entry)
	if err != nil {
		return nil, err
	}

	// put back non serialized properties
	res.Body = ioutil.NopCloser(bytes.NewReader(body))
	res.Request = req

	// determine timeout & put it in cache
	timeout := t.Config.MaxAge(res)
	if timeout != 0 {
		if err = t.ByteCache.Store(key, raw, timeout); err != nil {
			return nil, err
		}
	}

	// reset body in case the config.Timeout logic consumed it
	res.Body = ioutil.NopCloser(bytes.NewReader(body))
	return res, nil
}

type cacheByPath time.Duration

// Key returns the URL path (sans scheme and query params) to be used as the caching key
func (c cacheByPath) Key(req *http.Request) string {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return ""
	}
	return req.URL.Host + "/" + req.URL.Path
}

// MaxAge of a key is a preconfigured TTL
func (c cacheByPath) MaxAge(_ *http.Response) time.Duration {
	return time.Duration(c)
}

// CacheByPath caches against the host + path (ignoring scheme, auth, query etc) for the specified duration.
func CacheByPath(timeout time.Duration) Config {
	return cacheByPath(timeout)
}

type cacheByURL time.Duration

// Key returns the complete URL, to be used as the caching key
func (c cacheByURL) Key(req *http.Request) string {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return ""
	}
	return req.URL.String()
}

// MaxAge of a key is a preconfigured TTL
func (c cacheByURL) MaxAge(_ *http.Response) time.Duration {
	return time.Duration(c)
}

// CacheByURL caches against the entire URL for the specified duration.
func CacheByURL(timeout time.Duration) Config {
	return cacheByURL(timeout)
}
