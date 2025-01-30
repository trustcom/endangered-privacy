package app

import (
	"net/http"
	"net/url"

	"golang.org/x/net/publicsuffix"
	"karl/pkg/config"
)

func wrapRoundTripper(rt http.RoundTripper, config *config.AppConfig) http.RoundTripper {
	return &customRoundTripper{
		RoundTripper: rt,
		config:       config,
	}
}

type customRoundTripper struct {
	http.RoundTripper

	config *config.AppConfig
}

func (rt *customRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	h := req.Header.Clone()
	req = req.WithContext(req.Context())
	req.Header = h

	s := req.Header.Get("Origin")
	if s == "" {
		s = req.Header.Get("Referer")
	}

	u, err := url.Parse(s)
	if err == nil && u.Host != "" {
		setDefaultCORSHeaders(req, u)
	}

	for k, v := range defaultHeaders {
		setHeaderIfEmpty(req.Header, k, v)
	}

	if limiter := rt.config.RequestLimiter[req.URL.Hostname()]; limiter != nil {
		limiter.Wait(req.Context())
	}

	return rt.RoundTripper.RoundTrip(req)
}

// Some "best effort" browser-like headers to mitigate bot detection.
var (
	defaultHeaders = http.Header{
		"User-Agent":      {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6.1 Safari/605.1.15"},
		"Accept":          {"text/html", "application/xhtml+xml", "application/xml;q=0.9", "*/*;q=0.8"},
		"Accept-Language": {"en-gb"},
		"Sec-Fetch-Dest":  {"document"},
		"Sec-Fetch-Mode":  {"navigate"},
		"Sec-Fetch-Site":  {"none"},
	}

	defaultCORSHeaders = http.Header{
		"Accept":         {"*/*"},
		"Sec-Fetch-Dest": {"empty"},
		"Sec-Fetch-Mode": {"cors"},
	}
)

func setHeaderIfEmpty(header http.Header, key string, values []string) {
	if header.Get(key) == "" {
		header[key] = values
	}
}

func sameOrigin(u1, u2 *url.URL) bool {
	return u1.Scheme == u2.Scheme && u1.Host == u2.Host
}

func sameSite(u1, u2 *url.URL) bool {
	e1, err := publicsuffix.EffectiveTLDPlusOne(u1.Host)
	if err != nil {
		return false
	}

	e2, err := publicsuffix.EffectiveTLDPlusOne(u2.Host)
	if err != nil {
		return false
	}

	return e1 == e2
}

func setDefaultCORSHeaders(req *http.Request, origin *url.URL) {
	for k, v := range defaultCORSHeaders {
		setHeaderIfEmpty(req.Header, k, v)
	}

	if sameOrigin(req.URL, origin) {
		setHeaderIfEmpty(req.Header, "Sec-Fetch-Site", []string{"same-origin"})
		return
	}

	if sameSite(req.URL, origin) {
		setHeaderIfEmpty(req.Header, "Sec-Fetch-Site", []string{"same-site"})
		return
	}

	setHeaderIfEmpty(req.Header, "Sec-Fetch-Site", []string{"cross-site"})
}
