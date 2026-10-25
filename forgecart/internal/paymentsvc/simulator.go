// Package paymentsvc is ForgeCart's payment service: the payment providers (a
// simulator and Razorpay), the payment attempts table it owns and the HTTP API the
// order service calls. It knows nothing about orders beyond their ID and amount.
package paymentsvc

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"
)

const (
	OutcomeSucceeded        = "succeeded"
	OutcomeDeclined         = "declined"
	OutcomeTemporaryFailure = "temporary_failure"

	// MaxDelayLimit caps the configurable provider delay so a request cannot be
	// held open for long.
	MaxDelayLimit = 5 * time.Second
)

// The payment providers. The simulator is the default and is what the load
// tests use; Razorpay needs a person to pay on its hosted page.
const (
	ProviderSimulator = "simulator"
	ProviderRazorpay  = "razorpay"
)

// Config selects the simulated outcome mix. The three percentages must be
// 0-100 and sum to exactly 100.
type Config struct {
	SuccessPct, DeclinePct, TemporaryFailurePct int
	Seed                                        string
	MinDelay, MaxDelay                          time.Duration
	// Provider is ProviderSimulator (the default, when empty) or ProviderRazorpay.
	// The percentages, seed and delays above only apply to the simulator.
	Provider string
	Razorpay RazorpayConfig
}

func (c Config) Validate() error {
	for _, p := range []int{c.SuccessPct, c.DeclinePct, c.TemporaryFailurePct} {
		if p < 0 || p > 100 {
			return errors.New("each outcome percentage must be between 0 and 100")
		}
	}
	if c.SuccessPct+c.DeclinePct+c.TemporaryFailurePct != 100 {
		return errors.New("outcome percentages must sum to exactly 100")
	}
	if c.Seed == "" || len(c.Seed) > 64 {
		return errors.New("seed must be 1 to 64 characters")
	}
	if c.MinDelay < 0 || c.MaxDelay < c.MinDelay || c.MaxDelay > MaxDelayLimit {
		return errors.New("delays must satisfy 0 <= min <= max <= " + MaxDelayLimit.String())
	}
	return nil
}

// Decision is what the simulator picked for one logical attempt.
type Decision struct {
	Outcome           string
	Delay             time.Duration
	ProviderReference string // set only for a successful payment
}

// Decide is a pure function of the configuration, the order ID and the
// idempotency key: SHA-256 of seed, order and key gives a number in [0,100)
// for the outcome and another value for the delay. It does not use a random
// source, so the same inputs give the same decision in every process.
func (c Config) Decide(orderID, key string) Decision {
	sum := sha256.Sum256([]byte(c.Seed + "\x00" + orderID + "\x00" + key))
	bucket := int(binary.BigEndian.Uint64(sum[0:8]) % 100)
	d := Decision{}
	switch {
	case bucket < c.SuccessPct:
		d.Outcome = OutcomeSucceeded
		d.ProviderReference = "sim_" + hex.EncodeToString(sum[16:28])
	case bucket < c.SuccessPct+c.DeclinePct:
		d.Outcome = OutcomeDeclined
	default:
		d.Outcome = OutcomeTemporaryFailure
	}
	span := int64(c.MaxDelay-c.MinDelay) + 1
	d.Delay = c.MinDelay + time.Duration(int64(binary.BigEndian.Uint64(sum[8:16])%uint64(span)))
	if d.Delay > c.MaxDelay {
		d.Delay = c.MaxDelay
	}
	return d
}
