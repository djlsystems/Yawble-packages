package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// api is a Microsoft API the one connection holds a token for, keyed as stdin keys tokens.
type api string

const (
	apiARM     api = "https://management.azure.com/"
	apiSQL     api = "https://database.windows.net/"
	apiStorage api = "https://storage.azure.com/"
	apiVault   api = "https://vault.azure.net/"
	apiLogs    api = "https://api.loganalytics.io/"
)

var apiNames = map[api]string{
	apiARM:     "Azure management",
	apiSQL:     "Azure SQL",
	apiStorage: "Azure Storage",
	apiVault:   "Key Vault",
	apiLogs:    "Log Analytics",
}

// armBase is Azure Resource Manager's address.
const armBase = "https://management.azure.com"

// Where the plugin's requests go, and how long a long-running operation is followed. Only tests
// change them: endpoint maps each real address to a fake.
var (
	endpoint   = func(url string) string { return url }
	httpClient = &http.Client{Timeout: 60 * time.Second}
	pollEvery  = 3 * time.Second
	pollFor    = 50 * time.Minute
	sleep      = time.Sleep
)

// client sends every request the plugin makes, each with the token for its own API. Every API's
// token comes from the same sign-in; a request never carries another API's token.
type client struct {
	tokens   map[api]string
	progress func(string)
}

// apiError is an answer other than 2xx. Its text holds Azure's code and message, never a token:
// everything the plugin writes is scrubbed again.
type apiError struct {
	api     api
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	name := apiNames[e.api]
	switch {
	case e.code != "" && e.message != "":
		return fmt.Sprintf("%s answered %d %s: %s", name, e.status, e.code, e.message)
	case e.message != "":
		return fmt.Sprintf("%s answered %d: %s", name, e.status, e.message)
	}
	return fmt.Sprintf("%s answered %d.", name, e.status)
}

// isStatus reports whether err is an answer with that status.
func isStatus(err error, status int) bool {
	e, ok := err.(*apiError)
	return ok && e.status == status
}

// missingToken is the sentence for an API the connection holds no token for.
func missingToken(a api) error {
	return refusef("Refused: the Microsoft connection holds no token for %s. A person signs in on this member's microsoft connection slot (or reconnects it in Admin, Connections) and accepts every API it asks for.", apiNames[a])
}

func (c *client) has(a api) bool { return c.tokens[a] != "" }

// answer is a 2xx answer: its status, headers and body.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// send makes one request to an API with that API's token. A body that is not []byte is sent as
// JSON.
func (c *client) send(a api, method, url string, body any, headers map[string]string) (*answer, error) {
	token := c.tokens[a]
	if token == "" {
		return nil, missingToken(a)
	}
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("the request to %s could not be made", apiNames[a])
		}
		reader = bytes.NewReader(raw)
		contentType = "application/json"
	}
	req, err := http.NewRequest(method, endpoint(url), reader)
	if err != nil {
		return nil, fmt.Errorf("the request to %s could not be made", apiNames[a])
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s could not be reached; try again later", apiNames[a])
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &apiError{api: a, status: resp.StatusCode}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		var storage struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if json.Unmarshal(raw, &body) == nil {
			e.code, e.message = body.Error.Code, body.Error.Message
		} else if xml.Unmarshal(raw, &storage) == nil {
			// Azure Storage answers in XML; its message's later lines are request ids.
			e.code, _, _ = strings.Cut(storage.Code, "\n")
			e.message, _, _ = strings.Cut(storage.Message, "\n")
		}
		return nil, e
	}
	return &answer{status: resp.StatusCode, header: resp.Header, body: raw}, nil
}

// arm sends one request to Azure Resource Manager. path starts with /, and carries its api-version.
// A 2xx answer is decoded into into when it is not nil.
func (c *client) arm(method, path string, body, into any) (*answer, error) {
	ans, err := c.send(apiARM, method, armBase+path, body, nil)
	if err != nil {
		return nil, err
	}
	if into != nil && len(ans.body) > 0 {
		if err := json.Unmarshal(ans.body, into); err != nil {
			return nil, fmt.Errorf("Azure management's answer to %s could not be read", method)
		}
	}
	return ans, nil
}

// get reads an Azure Resource Manager path; found is false on 404.
func (c *client) get(path string, into any) (found bool, err error) {
	_, err = c.arm(http.MethodGet, path, nil, into)
	if isStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

// list reads every page of an Azure Resource Manager list.
func (c *client) list(path string, each func(json.RawMessage)) error {
	next := armBase + path
	for page := 0; next != "" && page < 200; page++ {
		ans, err := c.send(apiARM, http.MethodGet, next, nil, nil)
		if err != nil {
			return err
		}
		var body struct {
			Value    []json.RawMessage `json:"value"`
			NextLink string            `json:"nextLink"`
		}
		if json.Unmarshal(ans.body, &body) != nil {
			return fmt.Errorf("Azure management's list could not be read")
		}
		for _, v := range body.Value {
			each(v)
		}
		next = body.NextLink
	}
	return nil
}

// wait follows a long-running Azure Resource Manager operation that answered 201 or 202, until it
// ends, and answers the final body: the Azure-AsyncOperation status, or what Location answers last.
func (c *client) wait(first *answer, what string) ([]byte, error) {
	if first.status != http.StatusAccepted && first.status != http.StatusCreated {
		return first.body, nil
	}
	async := first.header.Get("Azure-AsyncOperation")
	location := first.header.Get("Location")
	if async == "" && location == "" {
		return first.body, nil
	}
	deadline := time.Now().Add(pollFor)
	delay := retryAfter(first.header)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s did not finish within %d minutes; it may still finish in Azure. Run show or list to see where it is", what, int(pollFor.Minutes()))
		}
		sleep(delay)
		if c.progress != nil {
			c.progress("azure: waiting for " + what)
		}
		if async != "" {
			ans, err := c.send(apiARM, http.MethodGet, async, nil, nil)
			if err != nil {
				return nil, err
			}
			var st struct {
				Status string `json:"status"`
				Error  struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(ans.body, &st)
			switch strings.ToLower(st.Status) {
			case "succeeded":
				if location != "" {
					if fin, err := c.send(apiARM, http.MethodGet, location, nil, nil); err == nil {
						return fin.body, nil
					}
				}
				return ans.body, nil
			case "failed", "canceled":
				return nil, fmt.Errorf("%s ended %s: %s %s", what, strings.ToLower(st.Status), st.Error.Code, st.Error.Message)
			}
			delay = retryAfter(ans.header)
			continue
		}
		ans, err := c.send(apiARM, http.MethodGet, location, nil, nil)
		if err != nil {
			return nil, err
		}
		if ans.status != http.StatusAccepted {
			return ans.body, nil
		}
		delay = retryAfter(ans.header)
	}
}

func retryAfter(h http.Header) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 && s <= 60 {
		if d := time.Duration(s) * time.Second; d > pollEvery {
			return d
		}
	}
	return pollEvery
}

// anonymous makes a request that carries no token: an address that holds its own short-lived
// permission, such as the upload and log addresses a registry hands out, or a release download.
// The address is never written out; the caller hides it first.
func anonymous(method, url string, body []byte, headers map[string]string, timeout time.Duration) (*answer, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint(url), reader)
	if err != nil {
		return nil, fmt.Errorf("the request could not be made")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{Transport: httpClient.Transport, Timeout: timeout}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the address Azure gave could not be reached; try again later")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("the address Azure gave answered %d", resp.StatusCode)
	}
	return &answer{status: resp.StatusCode, header: resp.Header, body: raw}, nil
}

// data sends one request to a data-plane API (storage, Key Vault, Log Analytics) with that API's
// token, and decodes a JSON answer into into when it is not nil.
func (c *client) data(a api, method, url string, body any, headers map[string]string, into any) (*answer, error) {
	ans, err := c.send(a, method, url, body, headers)
	if err != nil {
		return nil, err
	}
	if into != nil && len(ans.body) > 0 {
		if err := json.Unmarshal(ans.body, into); err != nil {
			return nil, fmt.Errorf("%s's answer could not be read", apiNames[a])
		}
	}
	return ans, nil
}
