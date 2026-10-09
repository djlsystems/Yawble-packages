package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// restClient is the JSON-over-HTTPS calls Gmail and Graph share. The token is only ever put in
// the Authorization header; an error carries the status and the provider's reason, never a header.
type restClient struct {
	base   string
	token  string
	client *http.Client
}

func newRESTClient(base, token string) *restClient {
	return &restClient{base: strings.TrimRight(base, "/"), token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

// apiError is the provider answering anything but 2xx.
type apiError struct {
	status  int
	reason  string // Gmail's errors[0].reason, or Graph's error.code
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("answered %d", e.status) }

// unreachable is the provider not answering at all.
type unreachable struct{}

func (unreachable) Error() string { return "could not be reached" }

// call sends one request. path is relative to the base, or an absolute link the provider gave
// (Graph's nextLink and deltaLink), which must lie under the base so the token goes nowhere else.
func (c *restClient) call(ctx context.Context, method, path string, query url.Values, body, into any, headers ...string) error {
	u := c.base + path
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		if !strings.HasPrefix(path, c.base+"/") {
			return fmt.Errorf("the stored position points outside the provider")
		}
		u = path
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return unreachable{}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// The error names the URL, which holds no token; it is still not passed on.
		return unreachable{}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &apiError{status: resp.StatusCode}
		var v struct {
			Error struct {
				Code    json.RawMessage `json:"code"`
				Message string          `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &v) == nil {
			e.message = v.Error.Message
			if len(v.Error.Errors) > 0 {
				e.reason = v.Error.Errors[0].Reason
			} else if s, err := strconv.Unquote(string(v.Error.Code)); err == nil {
				e.reason = s
			}
		}
		return e
	}
	if into != nil && len(data) > 0 && json.Unmarshal(data, into) != nil {
		return fmt.Errorf("the answer could not be read")
	}
	return nil
}

// httpWords are how one HTTP provider's failures are put.
type httpWords struct {
	provider  string // "Gmail", "Microsoft Graph"
	reconnect string // the sentence for a refused token
}

// describe turns a call's error into the run's failure words. scope is the scope the command
// needs; outward is true for draft and send, whose failures say that nothing went out.
func (w httpWords) describe(err error, scope string, outward bool) string {
	tail := ""
	if outward {
		tail = "; nothing was sent or drafted"
	}
	switch e := err.(type) {
	case *apiError:
		// Gmail answers a used-up quota with 403 and a reason naming the limit
		// (rateLimitExceeded, userRateLimitExceeded, dailyLimitExceeded, quotaExceeded): that is
		// "later", not a missing scope.
		reason := strings.ToLower(e.reason)
		rateLimited := e.status == http.StatusTooManyRequests || e.status >= 500 ||
			(e.status == http.StatusForbidden && (strings.Contains(reason, "limit") || strings.Contains(reason, "quota")))
		switch {
		case e.status == http.StatusUnauthorized:
			return w.reconnect
		case rateLimited:
			return fmt.Sprintf("%s answered %d %s; try again later%s.", w.provider, e.status, http.StatusText(e.status), tail)
		case e.status == http.StatusForbidden:
			return fmt.Sprintf("%s refused the request (403): the connection lacks the scope %s. A person reconnects the connection in Admin, Connections, granting that scope%s.", w.provider, scope, tail)
		default:
			msg := ""
			if e.message != "" {
				msg = ": " + e.message
			}
			return fmt.Sprintf("%s answered %d %s%s%s.", w.provider, e.status, http.StatusText(e.status), msg, tail)
		}
	case unreachable:
		return w.provider + " could not be reached; try again later" + tail + "."
	default:
		return w.provider + " " + err.Error() + tail + "."
	}
}

func isStatus(err error, status int) bool {
	e, ok := err.(*apiError)
	return ok && e.status == status
}
