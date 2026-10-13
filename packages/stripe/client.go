package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Where the plugin talks to. Only tests change them.
var (
	stripeBase = "https://api.stripe.com"
	armBase    = "https://management.azure.com"
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

// stripeVersion pins the shape of Stripe's answers, whatever the account's default version is.
const stripeVersion = "2024-06-20"

type stripeClient struct{ key string }

// apiError is an answer from Stripe other than 2xx. Its text never holds the key: Stripe's message
// is scrubbed again when it is written out.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("Stripe answered %d: %s", e.status, e.message)
	}
	return fmt.Sprintf("Stripe answered %d.", e.status)
}

func (e *apiError) notFound() bool { return e.status == http.StatusNotFound }

// isStatus reports whether err is a Stripe answer with that status.
func isStatus(err error, status int) bool {
	e, ok := err.(*apiError)
	return ok && e.status == status
}

// do sends one request and decodes a 2xx answer into into (when not nil). A POST carries an
// Idempotency-Key when idem is set, so a retried create cannot make a second object.
func (c *stripeClient) do(method, path string, form url.Values, idem string, into any) error {
	target := stripeBase + path
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return fmt.Errorf("the request to Stripe could not be made")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Stripe-Version", stripeVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Stripe could not be reached; try again later")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &apiError{status: resp.StatusCode, code: e.Error.Code, message: e.Error.Message}
	}
	if into != nil && json.Unmarshal(raw, into) != nil {
		return fmt.Errorf("Stripe's answer to %s %s could not be read", method, path)
	}
	return nil
}

var accountInMessage = regexp.MustCompile(`acct_[A-Za-z0-9]+`)

// accountID answers the account the key belongs to. A restricted key may not read /v1/account;
// Stripe's refusal names the account, so that is read instead.
func (c *stripeClient) accountID() (string, error) {
	var acct struct {
		ID string `json:"id"`
	}
	err := c.do(http.MethodGet, "/v1/account", nil, "", &acct)
	if err == nil && acct.ID != "" {
		return acct.ID, nil
	}
	if e, ok := err.(*apiError); ok {
		if e.status == http.StatusUnauthorized {
			return "", refusal("Refused: Stripe does not accept apiKey (401). It may have been deleted or rolled; make a new restricted key and bind it.")
		}
		if e.status == http.StatusForbidden {
			if m := accountInMessage.FindString(e.message); m != "" {
				return m, nil
			}
		}
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("Stripe did not say which account the key belongs to")
}
