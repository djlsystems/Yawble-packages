package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// A product's lookup key is its Stripe id, so ensuring it is one exact read, never a search that
// can lag behind a create. A price's lookup key is Stripe's own lookup_key.
var (
	lookupKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	currency  = regexp.MustCompile(`^[a-z]{3}$`)
)

const maxAmount = 99999999

type product struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Active      bool   `json:"active"`
}

type price struct {
	ID         string `json:"id"`
	Product    string `json:"product"`
	UnitAmount int64  `json:"unit_amount"`
	Currency   string `json:"currency"`
	Type       string `json:"type"`
	Active     bool   `json:"active"`
	LookupKey  string `json:"lookup_key"`
}

// idempotencyKey names one change by what it asks for, so the same command sent twice within
// Stripe's idempotency window makes one object.
func idempotencyKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "stripe-plugin-" + hex.EncodeToString(sum[:16])
}

func checkLookupKey(what, key string) error {
	if !lookupKey.MatchString(key) {
		return refusal(fmt.Sprintf("Refused: %s %q must be 1 to 64 letters, digits, - and _.", what, key))
	}
	return nil
}

// getProduct answers the product with that lookup key, or nil when there is none.
func (p *plugin) getProduct(key string) (*product, error) {
	var prod product
	err := p.api.do(http.MethodGet, "/v1/products/"+key, nil, "", &prod)
	if e, ok := err.(*apiError); ok && e.notFound() {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &prod, nil
}

// product ensure <lookup key> <name> [description]
func (p *plugin) productEnsure(args []string) (string, error) {
	if len(args) < 2 || len(args) > 3 {
		return "", refusal(`Refused: product ensure takes <lookup key> <name> [description]; quote a name or description with spaces ("Pro plan").`)
	}
	key, name, desc := args[0], strings.TrimSpace(args[1]), ""
	if len(args) == 3 {
		desc = strings.TrimSpace(args[2])
	}
	if err := checkLookupKey("the lookup key", key); err != nil {
		return "", err
	}
	if name == "" || len(name) > 250 {
		return "", refusal("Refused: the product's name must be 1 to 250 characters.")
	}

	existing, err := p.getProduct(key)
	if err != nil {
		return "", err
	}

	if existing == nil {
		if !p.applying() {
			return fmt.Sprintf("Would create product %s %q. %s", key, name, dryRunNote), nil
		}
		form := url.Values{"id": {key}, "name": {name}, "metadata[lookup_key]": {key}}
		if desc != "" {
			form.Set("description", desc)
		}
		var made product
		if err := p.api.do(http.MethodPost, "/v1/products", form, idempotencyKey("product", key, name, desc), &made); err != nil {
			return "", err
		}
		p.changed("product", made.ID, key, "created")
		return fmt.Sprintf("Created product %s %q.", made.ID, name), nil
	}

	descChanged := len(args) == 3 && desc != existing.Description
	if existing.Name == name && !descChanged && existing.Active {
		return fmt.Sprintf("Product %s %q is already there; nothing to change.", key, name), nil
	}
	if !p.applying() {
		return fmt.Sprintf("Would update product %s to name %q%s. %s", key, name, map[bool]string{true: " and its description", false: ""}[descChanged], dryRunNote), nil
	}
	form := url.Values{"name": {name}, "active": {"true"}}
	if descChanged {
		form.Set("description", desc)
	}
	if err := p.api.do(http.MethodPost, "/v1/products/"+existing.ID, form, idempotencyKey("product-update", key, name, desc), nil); err != nil {
		return "", err
	}
	p.changed("product", existing.ID, key, "updated")
	return fmt.Sprintf("Updated product %s %q.", existing.ID, name), nil
}

// price ensure <lookup key> <product lookup key> <amount in cents> <currency>
func (p *plugin) priceEnsure(args []string) (string, error) {
	if len(args) != 4 {
		return "", refusal("Refused: price ensure takes <lookup key> <product lookup key> <amount in cents> <currency>, such as: price ensure pro-monthly pro 1500 usd. One-time prices only.")
	}
	key, prodKey, currencyCode := args[0], args[1], strings.ToLower(args[3])
	if err := checkLookupKey("the lookup key", key); err != nil {
		return "", err
	}
	if err := checkLookupKey("the product lookup key", prodKey); err != nil {
		return "", err
	}
	amount, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || amount < 0 || amount > maxAmount {
		return "", refusal(fmt.Sprintf("Refused: the amount %q must be a whole number of the currency's smallest unit (cents), from 0 to %d.", args[2], maxAmount))
	}
	if !currency.MatchString(currencyCode) {
		return "", refusal(fmt.Sprintf("Refused: the currency %q must be a three-letter ISO code, such as usd or eur.", args[3]))
	}

	prod, err := p.getProduct(prodKey)
	if err != nil {
		return "", err
	}
	if prod == nil {
		if !p.applying() {
			return fmt.Sprintf("Would create price %s: %d %s, one-time, on product %s, once product %s exists (product ensure makes it). %s", key, amount, currencyCode, prodKey, prodKey, dryRunNote), nil
		}
		return "", refusal(fmt.Sprintf("Refused: there is no product %s; run product ensure %s <name> first.", prodKey, prodKey))
	}

	var list struct {
		Data []price `json:"data"`
	}
	if err := p.api.do(http.MethodGet, "/v1/prices", url.Values{"lookup_keys[]": {key}, "limit": {"1"}}, "", &list); err != nil {
		return "", err
	}
	var existing *price
	if len(list.Data) > 0 {
		existing = &list.Data[0]
	}

	what := fmt.Sprintf("%d %s, one-time, on product %s", amount, currencyCode, prod.ID)
	if existing != nil && existing.Product == prod.ID && existing.UnitAmount == amount &&
		existing.Currency == currencyCode && existing.Type == "one_time" && existing.Active {
		return fmt.Sprintf("Price %s (%s) is already there: %s; nothing to change.", key, existing.ID, what), nil
	}

	if !p.applying() {
		if existing != nil {
			return fmt.Sprintf("Would create a new price %s (%s) and move lookup key %s to it from %s, which would be archived. %s", key, what, key, existing.ID, dryRunNote), nil
		}
		return fmt.Sprintf("Would create price %s: %s. %s", key, what, dryRunNote), nil
	}

	form := url.Values{
		"product":     {prod.ID},
		"unit_amount": {strconv.FormatInt(amount, 10)},
		"currency":    {currencyCode},
		"lookup_key":  {key},
	}
	if existing != nil {
		// A price's amount cannot change; a new price takes the lookup key over.
		form.Set("transfer_lookup_key", "true")
	}
	var made price
	if err := p.api.do(http.MethodPost, "/v1/prices", form, idempotencyKey("price", key, prod.ID, form.Get("unit_amount"), currencyCode), &made); err != nil {
		return "", err
	}
	p.changed("price", made.ID, key, "created")
	if existing == nil {
		return fmt.Sprintf("Created price %s (%s): %s.", key, made.ID, what), nil
	}

	answer := fmt.Sprintf("Created price %s (%s): %s, and moved lookup key %s to it from %s.", key, made.ID, what, key, existing.ID)
	if existing.Active {
		if err := p.api.do(http.MethodPost, "/v1/prices/"+existing.ID, url.Values{"active": {"false"}}, "", nil); err != nil {
			return answer + " Archiving " + existing.ID + " failed (" + err.Error() + "); archive it in the Dashboard.", nil
		}
		p.changed("price", existing.ID, key, "updated")
		answer += " " + existing.ID + " is archived."
	}
	return answer, nil
}
