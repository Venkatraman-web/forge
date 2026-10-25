// Package config reads ForgeCart's settings from environment variables and
// validates them before anything is started.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Venkatraman-web/forge/forgecart/internal/appdb"
	"github.com/Venkatraman-web/forge/forgecart/internal/dispatcher"
	"github.com/Venkatraman-web/forge/forgecart/internal/paymentsvc"
	"github.com/Venkatraman-web/forge/sdk/workerclient"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

const (
	DatabaseURLEnv = "FORGECART_DATABASE_URL"
	AddrEnv        = "FORGECART_ADDR"
	// DefaultAddr is loopback only: ForgeCart has no authentication.
	DefaultAddr = "127.0.0.1:8081"
)

// Payment service variables. The payment service is a separate process
// (cmd/forgecart-payments) with its own database; the shop only calls it.
//
// Simulator variables (all optional) belong to the payment service. Defaults:
// every payment succeeds, no delay.
const (
	PaymentSuccessPctEnv = "FORGECART_PAYMENT_SUCCESS_PCT"      // default 100
	PaymentDeclinePctEnv = "FORGECART_PAYMENT_DECLINE_PCT"      // default 0
	PaymentTempFailEnv   = "FORGECART_PAYMENT_TEMP_FAILURE_PCT" // default 0
	PaymentSeedEnv       = "FORGECART_PAYMENT_SEED"             // default "forgecart-dev"
	PaymentMinDelayEnv   = "FORGECART_PAYMENT_MIN_DELAY"        // default 0s
	PaymentMaxDelayEnv   = "FORGECART_PAYMENT_MAX_DELAY"        // default 0s, at most 5s
)

// Payment provider. The default is the simulator; "razorpay" sends customers to
// a Razorpay payment link and needs the account's API keys (the secret is never
// logged or echoed). The base URL only changes to point at a stand-in server.
const (
	PaymentProviderEnv   = "FORGECART_PAYMENT_PROVIDER"    // simulator (default) or razorpay
	RazorpayKeyIDEnv     = "FORGECART_RAZORPAY_KEY_ID"     // required for razorpay
	RazorpayKeySecretEnv = "FORGECART_RAZORPAY_KEY_SECRET" // required for razorpay; a secret
	RazorpayBaseURLEnv   = "FORGECART_RAZORPAY_BASE_URL"   // default https://api.razorpay.com
	// The webhook listener receives Razorpay's payment link events. Set both together (and only with
	// the razorpay provider). Razorpay itself only calls ports 80 and 443 over HTTPS, so put a TLS
	// proxy on one of them in front of this listener.
	WebhookAddrEnv   = "FORGECART_PAYMENTS_WEBHOOK_ADDR"
	WebhookSecretEnv = "FORGECART_RAZORPAY_WEBHOOK_SECRET" // the secret set for the webhook in Razorpay's dashboard; a secret
	// The poller settles Razorpay links nobody asked about (see paymentsvc.PollPending).
	PollIntervalEnv = "FORGECART_RAZORPAY_POLL_INTERVAL" // default 30s; 0s turns it off
	PollBatchEnv    = "FORGECART_RAZORPAY_POLL_BATCH"    // links looked at per pass; default 50, at most 500
	// MinWebhookSecretLength is the shortest secret accepted.
	MinWebhookSecretLength = 12
)

// Optional metrics listener. Both settings are needed together: the listener
// serves business numbers, so it never runs without a key.
const (
	MetricsAddrEnv = "FORGECART_METRICS_ADDR"
	MetricsKeyEnv  = "FORGECART_METRICS_KEY" // a secret, at least MinMetricsKeyLength characters
	// MinMetricsKeyLength matches the length Forge requires of its own keys.
	MinMetricsKeyLength = 32
)

// Where the payment service listens and how the shop reaches it.
const (
	// DBMaxConnsEnv sets the database pool size of the shop and of the payment service (each reads it
	// in its own environment); every process of every service counts against PostgreSQL's connection limit.
	DBMaxConnsEnv          = "FORGECART_DB_MAX_CONNS"
	PaymentsDatabaseURLEnv = "FORGECART_PAYMENTS_DATABASE_URL" // the payment service's own database
	PaymentsAddrEnv        = "FORGECART_PAYMENTS_ADDR"         // the payment service's listen address
	DefaultPaymentsAddr    = "127.0.0.1:8095"
	PaymentURLEnv          = "FORGECART_PAYMENT_URL"     // the shop's setting: the payment service's base URL
	PaymentKeyEnv          = "FORGECART_PAYMENT_API_KEY" // shared bearer key, both sides; a secret
	// The shop's repair of payments the payment service recorded but the shop never applied.
	SweepIntervalEnv = "FORGECART_PAYMENT_SWEEP_INTERVAL" // default 30s; 0s turns the sweeper off
	// An order unpaid for this long is cancelled and its stock returned (see package payments).
	OrderExpiryEnv         = "FORGECART_ORDER_EXPIRY"          // default 30m; 0s turns expiry off; 1m to 30 days
	OrderExpiryIntervalEnv = "FORGECART_ORDER_EXPIRY_INTERVAL" // how often stale orders are looked for; default 30s
	SweepAgeEnv            = "FORGECART_PAYMENT_SWEEP_AGE"     // how long after a payment was recorded the sweeper may apply it; default 30s, at least 5s
	// MinPaymentKeyLength matches the metrics key.
	MinPaymentKeyLength = 32
)

type Config struct {
	DatabaseURL string
	Addr        string
	PaymentURL  string
	PaymentKey  string
	DBMaxConns  int
	// ExpiryAfter 0 means unpaid orders are never expired.
	ExpiryAfter    time.Duration
	ExpiryInterval time.Duration
	// SweepInterval 0 means the sweeper is off.
	SweepInterval time.Duration
	SweepAge      time.Duration
	// MetricsAddr and MetricsKey are empty when metrics are off.
	MetricsAddr string
	MetricsKey  string
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return n, nil
}

func envDuration(name string) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 250ms", name)
	}
	return d, nil
}

func loadPayment() (paymentsvc.Config, error) {
	var c paymentsvc.Config
	var err error
	if c.SuccessPct, err = envInt(PaymentSuccessPctEnv, 100); err != nil {
		return c, err
	}
	if c.DeclinePct, err = envInt(PaymentDeclinePctEnv, 0); err != nil {
		return c, err
	}
	if c.TemporaryFailurePct, err = envInt(PaymentTempFailEnv, 0); err != nil {
		return c, err
	}
	if c.Seed = os.Getenv(PaymentSeedEnv); c.Seed == "" {
		c.Seed = "forgecart-dev"
	}
	if c.MinDelay, err = envDuration(PaymentMinDelayEnv); err != nil {
		return c, err
	}
	if c.MaxDelay, err = envDuration(PaymentMaxDelayEnv); err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("invalid payment simulator settings: %v (see the FORGECART_PAYMENT_* variables)", err)
	}
	switch c.Provider = os.Getenv(PaymentProviderEnv); c.Provider {
	case "", paymentsvc.ProviderSimulator:
		c.Provider = paymentsvc.ProviderSimulator
	case paymentsvc.ProviderRazorpay:
		c.Razorpay = paymentsvc.RazorpayConfig{
			KeyID:     os.Getenv(RazorpayKeyIDEnv),
			KeySecret: os.Getenv(RazorpayKeySecretEnv),
			BaseURL:   os.Getenv(RazorpayBaseURLEnv),
		}
		if c.Razorpay.KeyID == "" || c.Razorpay.KeySecret == "" {
			return c, fmt.Errorf("%s=razorpay needs %s and %s", PaymentProviderEnv, RazorpayKeyIDEnv, RazorpayKeySecretEnv)
		}
		if c.Razorpay.BaseURL == "" {
			c.Razorpay.BaseURL = "https://api.razorpay.com"
		}
		u, err := url.Parse(c.Razorpay.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.User != nil {
			return c, fmt.Errorf("%s must be http(s)://host[:port] with no path or credentials", RazorpayBaseURLEnv)
		}
	default:
		return c, fmt.Errorf("%s must be %q or %q", PaymentProviderEnv, paymentsvc.ProviderSimulator, paymentsvc.ProviderRazorpay)
	}
	return c, nil
}

// Load validates the environment. Error messages name the variable but never
// echo its value, because the database URL contains a password.
func Load() (Config, error) {
	var c Config
	c.DatabaseURL = os.Getenv(DatabaseURLEnv)
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("%s is required (the ForgeCart application database; it must not be Forge's job database)", DatabaseURLEnv)
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return c, fmt.Errorf("%s must be a postgres:// or postgresql:// URL with a host", DatabaseURLEnv)
	}

	c.Addr = os.Getenv(AddrEnv)
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return c, fmt.Errorf("%s must be host:port", AddrEnv)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return c, errors.New(AddrEnv + " has an invalid port")
	}
	if host == "" {
		return c, fmt.Errorf("%s needs an explicit host (use 127.0.0.1 for local development)", AddrEnv)
	}
	c.MetricsAddr, c.MetricsKey = os.Getenv(MetricsAddrEnv), os.Getenv(MetricsKeyEnv)
	if (c.MetricsAddr == "") != (c.MetricsKey == "") {
		return c, fmt.Errorf("%s and %s must be set together (or both left unset)", MetricsAddrEnv, MetricsKeyEnv)
	}
	if c.MetricsAddr != "" {
		if _, mport, err := net.SplitHostPort(c.MetricsAddr); err != nil || mport == "" {
			return c, fmt.Errorf("%s must be host:port", MetricsAddrEnv)
		} else if mport == port {
			return c, fmt.Errorf("%s must differ from %s: metrics are served on their own listener", MetricsAddrEnv, AddrEnv)
		}
		if len(c.MetricsKey) < MinMetricsKeyLength {
			return c, fmt.Errorf("%s must be at least %d characters", MetricsKeyEnv, MinMetricsKeyLength)
		}
	}
	if c.PaymentURL = os.Getenv(PaymentURLEnv); c.PaymentURL == "" {
		return c, fmt.Errorf("%s is required (the payment service's address, for example http://127.0.0.1:8095)", PaymentURLEnv)
	}
	if u, err := url.Parse(c.PaymentURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.User != nil {
		return c, fmt.Errorf("%s must be http(s)://host[:port] with no path or credentials", PaymentURLEnv)
	}
	if c.PaymentKey = os.Getenv(PaymentKeyEnv); len(c.PaymentKey) < MinPaymentKeyLength {
		return c, fmt.Errorf("%s must be at least %d characters", PaymentKeyEnv, MinPaymentKeyLength)
	}
	if c.DBMaxConns, err = dbMaxConns(); err != nil {
		return c, err
	}
	if c.SweepInterval, err = envDurationDefault(SweepIntervalEnv, 30*time.Second); err != nil {
		return c, err
	}
	if c.SweepAge, err = envDurationDefault(SweepAgeEnv, 30*time.Second); err != nil {
		return c, err
	}
	if c.ExpiryAfter, err = envDurationDefault(OrderExpiryEnv, 30*time.Minute); err != nil {
		return c, err
	}
	if c.ExpiryInterval, err = envDurationDefault(OrderExpiryIntervalEnv, 30*time.Second); err != nil {
		return c, err
	}
	if c.ExpiryAfter != 0 && (c.ExpiryAfter < time.Minute || c.ExpiryAfter > 30*24*time.Hour) {
		return c, fmt.Errorf("%s must be 0s (off) or between 1m and 720h", OrderExpiryEnv)
	}
	if c.ExpiryInterval < time.Second || c.ExpiryInterval > time.Hour {
		return c, fmt.Errorf("%s must be between 1s and 1h", OrderExpiryIntervalEnv)
	}
	if c.SweepInterval != 0 && (c.SweepInterval < time.Second || c.SweepInterval > time.Hour) {
		return c, fmt.Errorf("%s must be 0s (off) or between 1s and 1h", SweepIntervalEnv)
	}
	if c.SweepAge < 5*time.Second || c.SweepAge > 24*time.Hour {
		return c, fmt.Errorf("%s must be between 5s and 24h", SweepAgeEnv)
	}
	return c, nil
}

// PaymentServiceConfig is the payment service process's settings.
type PaymentServiceConfig struct {
	DatabaseURL string
	DBMaxConns  int
	Addr        string
	APIKey      string
	Payment     paymentsvc.Config
	// MetricsAddr and MetricsKey are empty when metrics are off.
	MetricsAddr string
	MetricsKey  string
	// PollInterval 0 means the Razorpay poller is off.
	PollInterval time.Duration
	PollBatch    int
	// WebhookAddr and WebhookSecret are empty when the webhook is off.
	WebhookAddr   string
	WebhookSecret string
}

// LoadPaymentService validates the payment service environment. Error messages
// name the variable but never echo its value.
func LoadPaymentService() (PaymentServiceConfig, error) {
	var c PaymentServiceConfig
	c.DatabaseURL = os.Getenv(PaymentsDatabaseURLEnv)
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("%s is required (the payment service's own database; it must not be the shop's or Forge's)", PaymentsDatabaseURLEnv)
	}
	if u, err := url.Parse(c.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return c, fmt.Errorf("%s must be a postgres:// or postgresql:// URL with a host", PaymentsDatabaseURLEnv)
	}
	if c.Addr = os.Getenv(PaymentsAddrEnv); c.Addr == "" {
		c.Addr = DefaultPaymentsAddr
	}
	if host, port, err := net.SplitHostPort(c.Addr); err != nil || host == "" {
		return c, fmt.Errorf("%s must be host:port with an explicit host", PaymentsAddrEnv)
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return c, errors.New(PaymentsAddrEnv + " has an invalid port")
	}
	if c.APIKey = os.Getenv(PaymentKeyEnv); len(c.APIKey) < MinPaymentKeyLength {
		return c, fmt.Errorf("%s must be at least %d characters", PaymentKeyEnv, MinPaymentKeyLength)
	}
	var err error
	if c.DBMaxConns, err = dbMaxConns(); err != nil {
		return c, err
	}
	c.MetricsAddr, c.MetricsKey = os.Getenv(MetricsAddrEnv), os.Getenv(MetricsKeyEnv)
	if (c.MetricsAddr == "") != (c.MetricsKey == "") {
		return c, fmt.Errorf("%s and %s must be set together (or both left unset)", MetricsAddrEnv, MetricsKeyEnv)
	}
	if c.MetricsAddr != "" {
		if _, mport, err := net.SplitHostPort(c.MetricsAddr); err != nil || mport == "" {
			return c, fmt.Errorf("%s must be host:port", MetricsAddrEnv)
		}
		if c.MetricsAddr == c.Addr {
			return c, fmt.Errorf("%s must differ from %s: metrics are served on their own listener", MetricsAddrEnv, PaymentsAddrEnv)
		}
		if len(c.MetricsKey) < MinMetricsKeyLength {
			return c, fmt.Errorf("%s must be at least %d characters", MetricsKeyEnv, MinMetricsKeyLength)
		}
	}
	c.Payment, err = loadPayment()
	if err != nil {
		return c, err
	}
	if c.PollInterval, err = envDurationDefault(PollIntervalEnv, 30*time.Second); err != nil {
		return c, err
	}
	if c.PollBatch, err = envInt(PollBatchEnv, 50); err != nil {
		return c, err
	}
	if c.PollInterval != 0 && (c.PollInterval < time.Second || c.PollInterval > time.Hour) {
		return c, fmt.Errorf("%s must be 0s (off) or between 1s and 1h", PollIntervalEnv)
	}
	if c.PollBatch < 1 || c.PollBatch > 500 {
		return c, fmt.Errorf("%s must be between 1 and 500", PollBatchEnv)
	}
	c.WebhookAddr, c.WebhookSecret = os.Getenv(WebhookAddrEnv), os.Getenv(WebhookSecretEnv)
	if (c.WebhookAddr == "") != (c.WebhookSecret == "") {
		return c, fmt.Errorf("%s and %s must be set together (or both left unset)", WebhookAddrEnv, WebhookSecretEnv)
	}
	if c.WebhookAddr != "" {
		if c.Payment.Provider != paymentsvc.ProviderRazorpay {
			return c, fmt.Errorf("the webhook belongs to the razorpay provider: set %s=razorpay or leave %s unset", PaymentProviderEnv, WebhookAddrEnv)
		}
		if host, _, err := net.SplitHostPort(c.WebhookAddr); err != nil || host == "" {
			return c, fmt.Errorf("%s must be host:port with an explicit host", WebhookAddrEnv)
		}
		if c.WebhookAddr == c.Addr || c.WebhookAddr == c.MetricsAddr {
			return c, fmt.Errorf("%s must differ from %s and %s", WebhookAddrEnv, PaymentsAddrEnv, MetricsAddrEnv)
		}
		if len(c.WebhookSecret) < MinWebhookSecretLength {
			return c, fmt.Errorf("%s must be at least %d characters", WebhookSecretEnv, MinWebhookSecretLength)
		}
	}
	return c, nil
}

// dbMaxConns reads the pool size (1 to 200, default appdb.DefaultMaxConns).
func dbMaxConns() (int, error) {
	n, err := envInt(DBMaxConnsEnv, appdb.DefaultMaxConns)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 200 {
		return 0, fmt.Errorf("%s must be between 1 and 200", DBMaxConnsEnv)
	}
	return n, nil
}

// Dispatcher settings (the standalone outbox dispatcher process).
const (
	ForgeURLEnv           = "FORGECART_FORGE_URL"
	ForgeAPIKeyEnv        = "FORGECART_FORGE_API_KEY" // Forge's submit key (FORGE_API_KEY); a secret
	DispatcherPollEnv     = "FORGECART_DISPATCHER_POLL_INTERVAL"
	DispatcherBatchEnv    = "FORGECART_DISPATCHER_BATCH_SIZE"
	DispatcherLeaseEnv    = "FORGECART_DISPATCHER_LEASE_DURATION"
	DispatcherTimeoutEnv  = "FORGECART_DISPATCHER_HTTP_TIMEOUT"
	DispatcherAttemptsEnv = "FORGECART_DISPATCHER_MAX_ATTEMPTS"
)

type DispatcherConfig struct {
	DatabaseURL string
	Dispatcher  dispatcher.Config
}

func envDurationDefault(name string, def time.Duration) (time.Duration, error) {
	if os.Getenv(name) == "" {
		return def, nil
	}
	return envDuration(name)
}

// LoadDispatcher validates the dispatcher environment. Defaults: poll 1s,
// batch 10, lease 30s, HTTP timeout 10s, 8 attempts. The lease must exceed the
// HTTP timeout by at least 5s, which leaves room for the database updates that
// follow the response.
func LoadDispatcher() (DispatcherConfig, error) {
	var c DispatcherConfig
	c.DatabaseURL = os.Getenv(DatabaseURLEnv)
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("%s is required (the ForgeCart application database; it must not be Forge's job database)", DatabaseURLEnv)
	}
	if u, err := url.Parse(c.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return c, fmt.Errorf("%s must be a postgres:// or postgresql:// URL with a host", DatabaseURLEnv)
	}
	d := &c.Dispatcher
	d.ForgeURL = os.Getenv(ForgeURLEnv)
	if d.ForgeURL == "" {
		return c, fmt.Errorf("%s is required (for example http://127.0.0.1:8080)", ForgeURLEnv)
	}
	if d.ForgeAPIKey = os.Getenv(ForgeAPIKeyEnv); d.ForgeAPIKey == "" {
		return c, fmt.Errorf("%s is required (Forge's submit key, FORGE_API_KEY)", ForgeAPIKeyEnv)
	}
	var err error
	if d.PollInterval, err = envDurationDefault(DispatcherPollEnv, time.Second); err != nil {
		return c, err
	}
	if d.Lease, err = envDurationDefault(DispatcherLeaseEnv, 30*time.Second); err != nil {
		return c, err
	}
	if d.HTTPTimeout, err = envDurationDefault(DispatcherTimeoutEnv, 10*time.Second); err != nil {
		return c, err
	}
	if d.BatchSize, err = envInt(DispatcherBatchEnv, 10); err != nil {
		return c, err
	}
	if d.MaxAttempts, err = envInt(DispatcherAttemptsEnv, 8); err != nil {
		return c, err
	}
	switch {
	case d.PollInterval < 100*time.Millisecond || d.PollInterval > time.Minute:
		return c, fmt.Errorf("%s must be between 100ms and 1m", DispatcherPollEnv)
	case d.BatchSize < 1 || d.BatchSize > 100:
		return c, fmt.Errorf("%s must be between 1 and 100", DispatcherBatchEnv)
	case d.HTTPTimeout < 100*time.Millisecond || d.HTTPTimeout > 2*time.Minute:
		return c, fmt.Errorf("%s must be between 100ms and 2m", DispatcherTimeoutEnv)
	case d.Lease > time.Hour || d.Lease < d.HTTPTimeout+5*time.Second:
		return c, fmt.Errorf("%s must be at least %s longer than %s and at most 1h", DispatcherLeaseEnv, 5*time.Second, DispatcherTimeoutEnv)
	case d.MaxAttempts < 1 || d.MaxAttempts > 100:
		return c, fmt.Errorf("%s must be between 1 and 100", DispatcherAttemptsEnv)
	}
	return c, nil
}

// Inventory worker settings.
const (
	ForgeWorkerURLEnv = "FORGECART_FORGE_WORKER_URL"     // Forge's worker API listener (FORGE_WORKER_ADDR)
	ForgeWorkerKeyEnv = "FORGECART_FORGE_WORKER_API_KEY" // Forge's shared worker key; a secret
	WorkerNameEnv     = "FORGECART_WORKER_NAME"
	// Optional: a second database for the workers' result rows (invoices, fulfillment plans,
	// notification deliveries, inventory reconciliations). Unset = they live in the shop database.
	ResultsDatabaseURLEnv = "FORGECART_RESULTS_DATABASE_URL"
	WorkerConcurrencyEnv  = "FORGECART_WORKER_CONCURRENCY"
	// Optional tuning knobs of the Forge worker runtime (unset = the SDK default).
	WorkerPollWaitEnv      = "FORGECART_WORKER_POLL_WAIT"      // how long one claim request may wait for work
	WorkerShutdownGraceEnv = "FORGECART_WORKER_SHUTDOWN_GRACE" // how long to wait for running jobs when stopping
	WorkerJobTimeoutEnv    = "FORGECART_WORKER_JOB_TIMEOUT"    // worker-side cap on one attempt (can only shorten the job's own timeout)
)

type WorkerConfig struct {
	DatabaseURL        string
	ResultsDatabaseURL string // empty: use DatabaseURL
	ForgeURL           string
	APIKey             string
	Name               string
	Concurrency        int

	// Zero means "use the SDK default" (PollWait, ShutdownGrace) or "no worker-side cap" (JobTimeout).
	PollWait      time.Duration
	ShutdownGrace time.Duration
	JobTimeout    time.Duration

	// MetricsAddr and MetricsKey are empty when the worker's metrics are off.
	MetricsAddr string
	MetricsKey  string
}

// LoadWorker validates the worker environment. The API key is required but is
// never echoed in an error; the Forge client validates its format itself.
func LoadWorker(defaultName string) (WorkerConfig, error) {
	var c WorkerConfig
	c.DatabaseURL = os.Getenv(DatabaseURLEnv)
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("%s is required (the ForgeCart application database; it must not be Forge's job database)", DatabaseURLEnv)
	}
	if u, err := url.Parse(c.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return c, fmt.Errorf("%s must be a postgres:// or postgresql:// URL with a host", DatabaseURLEnv)
	}
	if c.ResultsDatabaseURL = os.Getenv(ResultsDatabaseURLEnv); c.ResultsDatabaseURL != "" {
		if u, err := url.Parse(c.ResultsDatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
			return c, fmt.Errorf("%s must be a postgres:// or postgresql:// URL with a host", ResultsDatabaseURLEnv)
		}
	}
	c.MetricsAddr, c.MetricsKey = os.Getenv(MetricsAddrEnv), os.Getenv(MetricsKeyEnv)
	if (c.MetricsAddr == "") != (c.MetricsKey == "") {
		return c, fmt.Errorf("%s and %s must be set together (or both left unset)", MetricsAddrEnv, MetricsKeyEnv)
	}
	if c.MetricsAddr != "" {
		if host, port, err := net.SplitHostPort(c.MetricsAddr); err != nil || host == "" || port == "" {
			return c, fmt.Errorf("%s must be host:port with an explicit host", MetricsAddrEnv)
		}
		if len(c.MetricsKey) < MinMetricsKeyLength {
			return c, fmt.Errorf("%s must be at least %d characters", MetricsKeyEnv, MinMetricsKeyLength)
		}
	}
	if c.ForgeURL = os.Getenv(ForgeWorkerURLEnv); c.ForgeURL == "" {
		return c, fmt.Errorf("%s is required (Forge's worker API address, for example http://127.0.0.1:8081)", ForgeWorkerURLEnv)
	}
	if c.APIKey = os.Getenv(ForgeWorkerKeyEnv); c.APIKey == "" {
		return c, fmt.Errorf("%s is required", ForgeWorkerKeyEnv)
	}
	if c.Name = os.Getenv(WorkerNameEnv); c.Name == "" {
		c.Name = defaultName
	}
	var err error
	if c.Concurrency, err = envInt(WorkerConcurrencyEnv, 4); err != nil {
		return c, err
	}
	// The ForgeCart pool has 10 connections; keep handlers below that.
	if c.Concurrency < 1 || c.Concurrency > workerruntime.MaxConcurrency {
		return c, fmt.Errorf("%s must be between 1 and %d", WorkerConcurrencyEnv, workerruntime.MaxConcurrency)
	}
	if c.PollWait, err = envDuration(WorkerPollWaitEnv); err != nil {
		return c, err
	}
	if c.PollWait != 0 && (c.PollWait < workerruntime.MinPollWait || c.PollWait > workerclient.MaxWait || c.PollWait%time.Second != 0) {
		return c, fmt.Errorf("%s must be a whole number of seconds from %s to %s", WorkerPollWaitEnv, workerruntime.MinPollWait, workerclient.MaxWait)
	}
	if c.ShutdownGrace, err = envDuration(WorkerShutdownGraceEnv); err != nil {
		return c, err
	}
	if c.ShutdownGrace < 0 || c.ShutdownGrace > workerruntime.MaxShutdownGrace {
		return c, fmt.Errorf("%s must be positive and at most %s", WorkerShutdownGraceEnv, workerruntime.MaxShutdownGrace)
	}
	if c.JobTimeout, err = envDuration(WorkerJobTimeoutEnv); err != nil {
		return c, err
	}
	if c.JobTimeout != 0 && c.JobTimeout < workerruntime.MinJobTimeout {
		return c, fmt.Errorf("%s must be at least %s", WorkerJobTimeoutEnv, workerruntime.MinJobTimeout)
	}
	return c, nil
}
