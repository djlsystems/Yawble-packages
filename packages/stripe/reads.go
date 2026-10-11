package main

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The read-only commands answer at most maxItems items. Each decodes only the fields it shows, so
// card details beyond brand and last four are never read into the plugin, let alone written out.
const maxItems = 50

var (
	objectID = regexp.MustCompile(`^[A-Za-z0-9_]{3,255}$`)
	email    = regexp.MustCompile(`^[^\s@'"]+@[^\s@'"]+\.[^\s@'"]+$`)
	ago      = regexp.MustCompile(`^([0-9]{1,4})([hd])$`)
)

func checkID(prefix, id string) error {
	if !objectID.MatchString(id) || !strings.HasPrefix(id, prefix) {
		return refusal(fmt.Sprintf("Refused: %q is not a Stripe id starting %s.", id, prefix))
	}
	return nil
}

func when(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

func money(amount int64, cur string) string {
	return fmt.Sprintf("%d %s", amount, cur)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// checkout show <cs_ id>
func (p *plugin) checkoutShow(args []string) (string, error) {
	if len(args) != 1 {
		return "", refusal("Refused: checkout show takes one Checkout Session id (cs_...).")
	}
	if err := checkID("cs_", args[0]); err != nil {
		return "", err
	}
	var s struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		PaymentStatus string `json:"payment_status"`
		Mode          string `json:"mode"`
		AmountTotal   int64  `json:"amount_total"`
		Currency      string `json:"currency"`
		Customer      string `json:"customer"`
		PaymentIntent string `json:"payment_intent"`
		Created       int64  `json:"created"`
	}
	if err := p.api.do(http.MethodGet, "/v1/checkout/sessions/"+args[0], nil, "", &s); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("there is no Checkout Session %s in this account", args[0])
		}
		return "", err
	}
	return strings.Join([]string{
		"checkout " + s.ID,
		"status " + orDash(s.Status) + ", payment " + orDash(s.PaymentStatus) + ", mode " + orDash(s.Mode),
		"amount " + money(s.AmountTotal, s.Currency),
		"customer " + orDash(s.Customer),
		"payment intent " + orDash(s.PaymentIntent),
		"created " + when(s.Created),
	}, "\n"), nil
}

// payment show <pi_ id>
func (p *plugin) paymentShow(args []string) (string, error) {
	if len(args) != 1 {
		return "", refusal("Refused: payment show takes one PaymentIntent id (pi_...).")
	}
	if err := checkID("pi_", args[0]); err != nil {
		return "", err
	}
	var pi struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		Amount       int64  `json:"amount"`
		Received     int64  `json:"amount_received"`
		Currency     string `json:"currency"`
		Customer     string `json:"customer"`
		Created      int64  `json:"created"`
		LatestCharge struct {
			ID      string `json:"id"`
			Details struct {
				Card *struct {
					Brand string `json:"brand"`
					Last4 string `json:"last4"`
				} `json:"card"`
			} `json:"payment_method_details"`
		} `json:"latest_charge"`
	}
	form := url.Values{"expand[]": {"latest_charge"}}
	if err := p.api.do(http.MethodGet, "/v1/payment_intents/"+args[0], form, "", &pi); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("there is no PaymentIntent %s in this account", args[0])
		}
		return "", err
	}
	card := "-"
	if c := pi.LatestCharge.Details.Card; c != nil {
		card = c.Brand + " ending " + c.Last4
	}
	return strings.Join([]string{
		"payment " + pi.ID,
		"status " + orDash(pi.Status),
		"amount " + money(pi.Amount, pi.Currency) + ", received " + money(pi.Received, pi.Currency),
		"customer " + orDash(pi.Customer),
		"charge " + orDash(pi.LatestCharge.ID) + ", card " + card,
		"created " + when(pi.Created),
	}, "\n"), nil
}

// customer find <email>
func (p *plugin) customerFind(args []string) (string, error) {
	if len(args) != 1 || !email.MatchString(args[0]) {
		return "", refusal("Refused: customer find takes one email address.")
	}
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			Email   string `json:"email"`
			Name    string `json:"name"`
			Created int64  `json:"created"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	form := url.Values{"email": {args[0]}, "limit": {strconv.Itoa(maxItems)}}
	if err := p.api.do(http.MethodGet, "/v1/customers", form, "", &list); err != nil {
		return "", err
	}
	if len(list.Data) == 0 {
		return "No customer has the email " + args[0] + ".", nil
	}
	var lines []string
	for i, c := range list.Data {
		if i == maxItems {
			break
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s  created %s", c.ID, c.Email, orDash(c.Name), when(c.Created)))
	}
	return listAnswer(fmt.Sprintf("%d customer(s) with the email %s", len(lines), args[0]), lines, list.HasMore), nil
}

// events [type] [since], since being a date (2026-01-31), a time (RFC 3339) or an age (24h, 7d).
func (p *plugin) events(args []string) (string, error) {
	if len(args) > 2 {
		return "", refusal("Refused: events takes [type] [since], such as: events checkout.session.completed 7d.")
	}
	form := url.Values{"limit": {strconv.Itoa(maxItems)}}
	var desc []string
	for _, a := range args {
		if since, ok := p.parseSince(a); ok {
			form.Set("created[gte]", strconv.FormatInt(since.Unix(), 10))
			desc = append(desc, "since "+since.Format(time.RFC3339))
			continue
		}
		if !eventName.MatchString(a) {
			return "", refusal(fmt.Sprintf("Refused: %q is neither an event type (checkout.session.completed, or checkout.session.* for a group) nor a since (2026-01-31, 24h, 7d).", a))
		}
		form.Set("type", a)
		desc = append(desc, "of type "+a)
	}
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Created int64  `json:"created"`
			Data    struct {
				Object struct {
					ID     string `json:"id"`
					Object string `json:"object"`
				} `json:"object"`
			} `json:"data"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	if err := p.api.do(http.MethodGet, "/v1/events", form, "", &list); err != nil {
		return "", err
	}
	title := "events"
	if len(desc) > 0 {
		title += " " + strings.Join(desc, " ")
	}
	if len(list.Data) == 0 {
		return "No " + title + ".", nil
	}
	var lines []string
	for i, e := range list.Data {
		if i == maxItems {
			break
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s %s", when(e.Created), e.Type, e.ID, orDash(e.Data.Object.Object), orDash(e.Data.Object.ID)))
	}
	return listAnswer(fmt.Sprintf("%d %s, newest first", len(lines), title), lines, list.HasMore), nil
}

func (p *plugin) parseSince(a string) (time.Time, bool) {
	if m := ago.FindStringSubmatch(a); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := time.Hour
		if m[2] == "d" {
			unit = 24 * time.Hour
		}
		return p.now().Add(-time.Duration(n) * unit).UTC().Truncate(time.Second), true
	}
	if t, err := time.Parse("2006-01-02", a); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, a); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func listAnswer(title string, lines []string, more bool) string {
	out := title + ":\n" + strings.Join(lines, "\n")
	if more || len(lines) == maxItems {
		out += fmt.Sprintf("\nAt most %d are shown; narrow it to see others.", maxItems)
	}
	return out
}
