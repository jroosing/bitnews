package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/redact"
)

// DefaultFetchTimeout is the total time an NZB URL fetch may take.
const DefaultFetchTimeout = 30 * time.Second

// maxRedirects is how many redirects a fetch follows.
const maxRedirects = 5

// fetchUserAgent identifies bitnews to indexers.
const fetchUserAgent = "bitnews (+https://github.com/jroosing/bitnews)"

// newFetchClient builds the client that fetches NZB URLs.
//
// SSRF policy: bitnews is a LAN download tool and Prowlarr or an indexer is
// often on a private address (http://prowlarr:9696, http://192.168.1.5), so
// private, loopback and link-local targets are deliberately allowed. What
// is restricted is the protocol: http and https only, also across
// redirects. Callers of the API are authenticated with a write key or a
// session.
//
// The client sends no ambient credentials: no cookie jar, no proxy from the
// environment (the Usenet connections go direct as well) and no netrc. Only
// credentials written in the URL itself are used.
func newFetchClient(o options) *http.Client {
	c := &http.Client{}
	if o.httpClient != nil {
		cp := *o.httpClient
		c = &cp
	}
	if c.Transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		c.Transport = t
	}
	c.Jar = nil
	c.CheckRedirect = checkRedirect
	c.Timeout = o.fetchTimeout
	return c
}

// checkRedirect allows maxRedirects redirects, to http and https only.
// Its messages never contain the URL: it may carry an API key.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if s := req.URL.Scheme; s != "http" && s != "https" {
		return fmt.Errorf("refused a redirect to a %q URL: only http and https are allowed", s)
	}
	return nil
}

// checkRedirectOrMagnet is checkRedirect, except that a redirect to a
// "magnet:" URI ends the fetch without error: the client then returns the
// redirect response itself, whose Location header holds the link. Indexer
// proxies such as Prowlarr and Jackett answer a torrent download that way
// when the release only has a magnet link.
func checkRedirectOrMagnet(req *http.Request, via []*http.Request) error {
	if strings.EqualFold(req.URL.Scheme, "magnet") {
		return http.ErrUseLastResponse
	}
	return checkRedirect(req, via)
}

// FetchNZB downloads the NZB at rawURL and stages it, within the fetch
// timeout and the NZB size cap. The URL is never stored, and only its
// redacted form is logged or returned in an error. Failures to reach the
// server, a non-200 answer and a timeout are errors of kind ErrUpstream
// (code "fetch_failed"); content that is not an NZB is "invalid_nzb".
func (s *AddService) fetchNZB(ctx context.Context, rawURL string) (*StagedNZB, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, invalid("invalid_url", "url must be an absolute http or https URL")
	}
	log := s.opts.log.With("url", redact.URL(rawURL))

	ctx, cancel := context.WithTimeout(ctx, s.opts.fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, invalid("invalid_url", "url must be an absolute http or https URL")
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	req.Header.Set("Accept", "application/x-nzb, text/xml;q=0.9, */*;q=0.1")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, s.fetchFailed(ctx, log, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Warn("nzb fetch failed", "status", resp.StatusCode)
		return nil, upstream("fetch_failed", "could not fetch the NZB: the server answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if resp.ContentLength > s.opts.maxNZBBytes {
		return nil, tooLarge("nzb_too_large", "the NZB is larger than %d MiB", s.opts.maxNZBBytes>>20)
	}

	body := &errRecorder{r: resp.Body}
	st, err := s.stage(ctx, body)
	if err != nil {
		if _, isApp := errors.AsType[*Error](err); !isApp && body.err != nil && !errors.Is(body.err, io.EOF) {
			// The failure was in the transfer, not in our disk or the
			// content.
			return nil, s.fetchFailed(ctx, log, body.err)
		}
		return nil, err
	}
	log.Info("nzb fetched")
	return st, nil
}

// errRecorder remembers the first error its reader returned.
type errRecorder struct {
	r   io.Reader
	err error
}

func (e *errRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

// fetchFailed turns a transport error into the API error. The error text of
// net/http quotes URLs, so every URL in it is redacted.
func (s *AddService) fetchFailed(ctx context.Context, log *slog.Logger, err error) error {
	return s.fetchFailedOf(ctx, log, "NZB", err)
}

// fetchFailedOf is fetchFailed for a fetch of what ("NZB", "NZB or torrent").
func (s *AddService) fetchFailedOf(ctx context.Context, log *slog.Logger, what string, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return err // the caller went away: not an upstream problem
	}
	var reason string
	switch ne, isNet := errors.AsType[net.Error](err); {
	case errors.Is(err, context.DeadlineExceeded), isNet && ne.Timeout():
		reason = fmt.Sprintf("timed out after %s", s.opts.fetchTimeout)
	default:
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		reason = redact.URLsIn(err.Error())
		reason = strings.TrimSpace(reason)
	}
	log.Warn(strings.ToLower(what)+" fetch failed", "reason", reason)
	return upstream("fetch_failed", "could not fetch the %s: %s", what, reason)
}
