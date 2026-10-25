// Package orders implements ForgeCart checkout: request validation, the
// stock-reserving transaction, and idempotent replay.
package orders

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/mail"
	"regexp"
	"sort"
	"strings"

	"github.com/Venkatraman-web/forge/forgecart/internal/catalog"
)

const (
	MaxItems     = 20  // distinct SKUs per order
	MaxQuantity  = 100 // units of one SKU per order
	MaxEmailLen  = 254
	SupportedCcy = "USD" // the only currency this reference app accepts
)

// Kind groups errors by how the HTTP layer should report them.
type Kind int

const (
	KindInvalid  Kind = iota // 400: the request itself is wrong
	KindNotFound             // 404: a requested product is unknown or inactive
	KindConflict             // 409: valid request, but state forbids it
)

// Error is a safe, client-presentable failure. Message never contains database text.
type Error struct {
	Kind    Kind
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func invalid(code, msg string) *Error { return &Error{KindInvalid, code, msg} }

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// ValidateKey checks an Idempotency-Key value (mirrors the table's CHECK).
func ValidateKey(key string) *Error {
	if !keyPattern.MatchString(key) {
		return invalid("invalid_idempotency_key", "Idempotency-Key must be 8 to 128 characters from A-Z a-z 0-9 _ . : -")
	}
	return nil
}

type Item struct {
	SKU      string
	Quantity int
}

// Request is a validated, normalized checkout: email trimmed and lower-cased,
// items sorted by SKU, no duplicate SKUs.
type Request struct {
	Email string
	Items []Item
}

// NewRequest validates and normalizes client input. Duplicate SKUs are
// rejected (not merged), so one purchase has exactly one representation.
func NewRequest(email string, items []Item) (Request, *Error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return Request{}, invalid("invalid_email", "customer_email is required.")
	}
	addr, err := mail.ParseAddress(email)
	at := strings.LastIndex(email, "@")
	if err != nil || addr.Address != email || len(email) > MaxEmailLen || at < 1 || !strings.Contains(email[at+1:], ".") {
		return Request{}, invalid("invalid_email", "customer_email must be a plain email address such as name@example.com.")
	}
	if len(items) == 0 {
		return Request{}, invalid("invalid_items", "items must contain at least one item.")
	}
	if len(items) > MaxItems {
		return Request{}, invalid("invalid_items", fmt.Sprintf("items may contain at most %d distinct products.", MaxItems))
	}
	out := make([]Item, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		sku := strings.TrimSpace(it.SKU)
		if !catalog.SKUPattern.MatchString(sku) {
			return Request{}, invalid("invalid_sku", "Each item needs a valid sku.")
		}
		if it.Quantity < 1 || it.Quantity > MaxQuantity {
			return Request{}, invalid("invalid_quantity", fmt.Sprintf("quantity must be an integer from 1 to %d.", MaxQuantity))
		}
		if seen[sku] {
			return Request{}, invalid("duplicate_item", "Each sku may appear only once; combine quantities into one item.")
		}
		seen[sku] = true
		out = append(out, Item{SKU: sku, Quantity: it.Quantity})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return Request{Email: email, Items: out}, nil
}

// Fingerprint is the SHA-256 of a canonical text form of the request: the
// email and the sorted SKU=quantity pairs. Prices and stock are server state
// and deliberately excluded, so the same purchase fingerprints identically
// after a price change.
func (r Request) Fingerprint() string {
	var b strings.Builder
	b.WriteString("forgecart-checkout-v1\n")
	b.WriteString(r.Email + "\n")
	for _, it := range r.Items {
		fmt.Fprintf(&b, "%s=%d\n", it.SKU, it.Quantity)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
