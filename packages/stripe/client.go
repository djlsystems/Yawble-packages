package main

import (
	"bytes"
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
	vaultBase  = func(vault string) string { return "https://" + vault + ".vault.azure.net" }
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

// stripeVersion pins the shape of Stripe's answers, whatever the account's default version is.
const stripeVersion = "2024-06-20"

// vaultAPIVersion is the Key Vault REST API version the plugin speaks.
const vaultAPIVersion = "7.4"

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

type vaultClient struct{ token string }

var versionInID = regexp.MustCompile(`/secrets/[^/]+/([^/?]+)$`)

// setSecret writes value as a new version of the secret and answers that version. The value goes
// only into the request body; the answer, which echoes it, is read for its id and nothing else.
func (v *vaultClient) setSecret(vault, name, value, contentType string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"value":       value,
		"contentType": contentType,
		"tags":        map[string]string{"writtenBy": "stripe-plugin"},
	})
	status, raw, err := v.send(http.MethodPut, vault, name, body)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusForbidden:
		return "", fmt.Errorf("Key Vault %s does not let the signed-in account set secrets (403); give that account the Key Vault Secrets Officer role on the vault, or an access policy with Set on secrets", vault)
	case status == http.StatusNotFound:
		return "", fmt.Errorf("Key Vault %s was not found (404); check its name in keyVaults", vault)
	case status/100 != 2:
		return "", fmt.Errorf("Key Vault %s answered %d", vault, status)
	}

	var answer struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return "", fmt.Errorf("Key Vault %s's answer could not be read", vault)
	}
	m := versionInID.FindStringSubmatch(answer.ID)
	if m == nil {
		return "", fmt.Errorf("Key Vault %s did not name the new version", vault)
	}
	return m[1], nil
}

// reach proves the vault answers the Microsoft sign-in with a read of one secret, changing nothing.
// The secret need not exist: a 404 still means the vault was reached. Its value is never read.
func (v *vaultClient) reach(vault, name string) error {
	status, _, err := v.send(http.MethodGet, vault, name, nil)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusForbidden:
		return fmt.Errorf("Key Vault %s does not let the signed-in account read or set secrets (403); give that account the Key Vault Secrets Officer role on the vault, or an access policy with Get and Set on secrets", vault)
	case status == http.StatusNotFound, status/100 == 2:
		return nil
	default:
		return fmt.Errorf("Key Vault %s answered %d", vault, status)
	}
}

// send makes one request to a vault's secret and answers the status and body. A refused sign-in
// (401) is an error here, the same for every request.
func (v *vaultClient) send(method, vault, name string, body []byte) (int, []byte, error) {
	if v.token == "" {
		return 0, nil, refusal("Refused: no Microsoft connection is bound to the microsoft slot, so nothing can be written to Key Vault. A person binds one in this member's settings.")
	}
	target := vaultBase(vault) + "/secrets/" + url.PathEscape(name) + "?api-version=" + vaultAPIVersion
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("the request to Key Vault could not be made")
	}
	req.Header.Set("Authorization", "Bearer "+v.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("Key Vault %s could not be reached; check the vault's name and its network rules", vault)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return 0, nil, fmt.Errorf("Key Vault %s refused the Microsoft sign-in (401); reconnect the microsoft connection from Admin, Connections", vault)
	}
	return resp.StatusCode, raw, nil
}
