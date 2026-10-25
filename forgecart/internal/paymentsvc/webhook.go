package paymentsvc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// maxWebhookBody bounds what an unauthenticated caller can make the handler read; a real
	// payment link event is about 2 KB.
	maxWebhookBody = 64 << 10
	// webhookBudget keeps the handler inside Razorpay's 5 second limit: a request that is
	// still running after that is treated as undelivered and sent again anyway.
	webhookBudget = 4 * time.Second
)

// Webhook receives Razorpay's payment link events. It is served on its own listener, because it
// is the one endpoint of the payment service that the internet has to reach, and it has no
// bearer key: the proof that a request is Razorpay's is the signature.
type Webhook struct {
	svc    *Service
	secret []byte
	log    *slog.Logger
	m      *metrics
}

func NewWebhook(api *API, secret string) *Webhook {
	return &Webhook{svc: api.svc, secret: []byte(secret), log: api.log, m: api.m}
}

func (w *Webhook) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhooks/razorpay", w.receive)
	return mux
}

// event is the part of a Razorpay event the handler reads. Only the order ID (the link's
// reference_id) is taken from it; everything else is read from Razorpay's API.
type event struct {
	Event   string `json:"event"`
	Payload struct {
		PaymentLink struct {
			Entity struct {
				ReferenceID string `json:"reference_id"`
			} `json:"entity"`
		} `json:"payment_link"`
	} `json:"payload"`
}

// signatureValid checks X-Razorpay-Signature: the hex HMAC-SHA256 of the raw body under the
// webhook secret, compared in constant time.
func (w *Webhook) signatureValid(body []byte, signature string) bool {
	mac := hmac.New(sha256.New, w.secret)
	mac.Write(body)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(signature)
	return err == nil && hmac.Equal(got, want)
}

func (w *Webhook) receive(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", http.MethodPost)
		w.reply(rw, http.StatusMethodNotAllowed, "bad_request")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxWebhookBody))
	if err != nil {
		w.reply(rw, http.StatusRequestEntityTooLarge, "bad_request")
		return
	}
	if !w.signatureValid(body, r.Header.Get("X-Razorpay-Signature")) {
		w.reply(rw, http.StatusUnauthorized, "bad_signature")
		return
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		w.reply(rw, http.StatusBadRequest, "bad_request")
		return
	}
	// Only the payment link events that can end a payment matter; anything else (partial
	// payments, other products of the same account) is acknowledged and ignored, so Razorpay
	// does not retry it.
	orderID := ev.Payload.PaymentLink.Entity.ReferenceID
	switch ev.Event {
	case "payment_link.paid", "payment_link.expired", "payment_link.cancelled":
	default:
		w.reply(rw, http.StatusOK, "ignored")
		return
	}
	if !orderIDPattern.MatchString(orderID) {
		w.reply(rw, http.StatusOK, "ignored")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), webhookBudget)
	defer cancel()
	settled, err := w.svc.Settle(ctx, orderID)
	switch {
	case errors.Is(err, ErrAmountMismatch):
		// Retrying cannot fix this; say so loudly and stop Razorpay from retrying.
		w.log.Error("webhook: the link's amount differs from the order's earlier attempts", "order_id", orderID, "event_id", r.Header.Get("X-Razorpay-Event-Id"))
		w.reply(rw, http.StatusOK, "mismatch")
	case err != nil:
		// 5xx makes Razorpay deliver the event again (with backoff, for 24 hours).
		w.log.Warn("webhook: could not settle the payment", "order_id", orderID, "cause", errClass(err))
		w.reply(rw, http.StatusServiceUnavailable, "error")
	default:
		w.log.Info("webhook", "event", ev.Event, "event_id", r.Header.Get("X-Razorpay-Event-Id"), "order_id", orderID, "settled", settled)
		w.reply(rw, http.StatusOK, "ok")
	}
}

// reply counts the result and answers with a tiny body (nothing from the request is echoed).
func (w *Webhook) reply(rw http.ResponseWriter, status int, result string) {
	w.m.webhooks.WithLabelValues(result).Inc()
	writeJSON(rw, status, map[string]string{"status": result})
}
