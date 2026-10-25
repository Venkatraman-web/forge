// Command forge starts the Forge job server: the submission API, the worker
// API (customers' workers claim jobs through it), the lease reaper and the
// optional operator API. It shuts them down in order on Ctrl-C.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Venkatraman-web/forge/internal/api"
	"github.com/Venkatraman-web/forge/internal/metrics"
	"github.com/Venkatraman-web/forge/internal/operatorapi"
	"github.com/Venkatraman-web/forge/internal/policy"
	"github.com/Venkatraman-web/forge/internal/store"
	"github.com/Venkatraman-web/forge/internal/worker"
	"github.com/Venkatraman-web/forge/internal/workerapi"
)

const (
	// Fixed settings.
	executionTimeout = 30 * time.Second
	shutdownTimeout  = 10 * time.Second
	// workerDrainTimeout bounds how long shutdown waits for handlers to return
	// and for their jobs to be released (the release itself may retry for up to
	// ~17s when the database is struggling).
	workerDrainTimeout = 30 * time.Second

	// Database settings.
	dbStartupTimeout = 5 * time.Second // limit for the connectivity check at startup
	// The pool is capped so a burst of requests cannot open more PostgreSQL
	// connections than the server allows (100 by default). Forge needs few:
	// one per busy worker plus one per in-flight HTTP request.
	dbMaxConns = 10

	// Defaults for the environment variables.
	defaultAddr = "127.0.0.1:8080" // local only: the key is a shared secret over plain HTTP
	// defaultMaxQueued is the global limit on jobs waiting to run.
	defaultMaxQueued = 1000
	// defaultMaxJobTimeout is the largest execution timeout a submitter may ask
	// for unless the operator raises FORGE_MAX_JOB_TIMEOUT. It equals the default
	// timeout, so a stock server behaves as before: nothing longer is accepted.
	defaultMaxJobTimeout = executionTimeout
	// defaultPollInterval is how long an idle worker waits between asking the
	// database for work.
	defaultPollInterval = time.Second

	// Lease timing. These defaults were chosen because the Task 20 experiments
	// ran them successfully; they are starting points, not universal values.
	// A 15s lease is enough for a 30s handler because a running job renews its
	// lease every 5s. The handler is cancelled if no renewal has succeeded for
	// 10s, 5s before the lease can expire.
	defaultLeaseDuration     = 15 * time.Second
	defaultHeartbeatInterval = 5 * time.Second
	defaultSafetyMargin      = 10 * time.Second
	defaultReaperInterval    = 5 * time.Second
	defaultReaperBatch       = 100
	// How often the retention purge looks for finished jobs past their retention time.
	defaultRetentionInterval = 10 * time.Second
	defaultRecoveryBackoff   = 2 * time.Second
	defaultRetryBaseDelay    = 2 * time.Second
	defaultRetryMaxDelay     = 5 * time.Minute
	// defaultWorkerMaxPolls bounds how many remote claim requests may wait at once.
	defaultWorkerMaxPolls = 64
)

func main() {
	// run does the work so its deferred cleanup always executes; os.Exit
	// would skip defers if called inside run.
	if err := run(); err != nil {
		log.Printf("forge: %v", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	addr := os.Getenv("FORGE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	// Forge starts no workers of its own: customers run theirs (built with
	// sdk/workerruntime) and they claim jobs through the worker API. The two
	// settings below belonged to the removed in-process mode; they are
	// refused, not ignored, so an old configuration is never silently different.
	if value, set := os.LookupEnv("FORGE_EXECUTION_MODE"); set && value != "remote" {
		return fmt.Errorf("FORGE_EXECUTION_MODE=%q is no longer supported: Forge always runs with remote workers (unset it, or set it to \"remote\")", value)
	}
	if _, set := os.LookupEnv("FORGE_WORKERS"); set {
		return errors.New("FORGE_WORKERS was removed: Forge starts no workers of its own; run your own with sdk/workerruntime")
	}

	// The job types come from the config file when there is one (loaded below,
	// once the timeout cap is known); otherwise from FORGE_REMOTE_JOB_TYPES.
	var remoteTypes []string
	if _, set := os.LookupEnv("FORGE_CONFIG_FILE"); set {
		if _, both := os.LookupEnv("FORGE_REMOTE_JOB_TYPES"); both {
			return errors.New("FORGE_REMOTE_JOB_TYPES cannot be set together with FORGE_CONFIG_FILE: the file declares the job types")
		}
	} else {
		var err error
		remoteTypes, err = worker.ParseRemoteJobTypes(os.Getenv("FORGE_REMOTE_JOB_TYPES"))
		if err != nil {
			return fmt.Errorf("FORGE_REMOTE_JOB_TYPES (required, or use FORGE_CONFIG_FILE): %w", err)
		}
	}
	// The worker API must never start unauthenticated or on the same address as
	// the submission API.
	workerAddr := strings.TrimSpace(os.Getenv("FORGE_WORKER_ADDR"))
	if workerAddr == "" {
		return errors.New("FORGE_WORKER_ADDR is required (for example 127.0.0.1:8081)")
	}
	if workerAddr == addr {
		return errors.New("FORGE_WORKER_ADDR must differ from FORGE_ADDR: the worker API has its own listener")
	}
	// Each API has its own keys (see keysFromEnv): whoever submits jobs, works them or reads
	// operator data must be authorised, and no key may serve two roles.
	submitKeys, workerKeys, operatorKeys, err := keysFromEnv()
	if err != nil {
		return err
	}
	workerMaxPoll, err := positiveIntFromEnv("FORGE_WORKER_MAX_POLLS", defaultWorkerMaxPolls)
	if err != nil {
		return err
	}
	// The optional operator API (read-only queue status). It is off unless BOTH
	// settings are given, and it never shares an address or a key with the other
	// two APIs.
	operatorAddr, err := operatorFromEnv(addr, workerAddr, !operatorKeys.Empty())
	if err != nil {
		return err
	}
	maxQueued, err := positiveIntFromEnv("FORGE_MAX_QUEUED", defaultMaxQueued)
	if err != nil {
		return err
	}
	maxJobTimeout, err := durationFromEnv("FORGE_MAX_JOB_TIMEOUT", defaultMaxJobTimeout)
	if err != nil {
		return err
	}
	if _, err := worker.ValidateMaxTimeout(maxJobTimeout); err != nil {
		return fmt.Errorf("FORGE_MAX_JOB_TIMEOUT: %w", err)
	}
	var typePolicy *policy.Policy
	if path, set := os.LookupEnv("FORGE_CONFIG_FILE"); set {
		typePolicy, err = policy.Load(path, maxJobTimeout)
		if err != nil {
			return fmt.Errorf("FORGE_CONFIG_FILE: %w", err)
		}
		remoteTypes = typePolicy.TypeNames()
		log.Printf("forge: config file loaded: %d job type(s): %s", len(remoteTypes), strings.Join(remoteTypes, ", "))
	}
	pollInterval, err := durationFromEnv("FORGE_POLL_INTERVAL", defaultPollInterval)
	if err != nil {
		return err
	}
	leaseDuration, err := durationFromEnv("FORGE_LEASE_DURATION", defaultLeaseDuration)
	if err != nil {
		return err
	}
	heartbeat, err := durationFromEnv("FORGE_HEARTBEAT_INTERVAL", defaultHeartbeatInterval)
	if err != nil {
		return err
	}
	safetyMargin, err := durationFromEnv("FORGE_LEASE_SAFETY_MARGIN", defaultSafetyMargin)
	if err != nil {
		return err
	}
	reaperInterval, err := durationFromEnv("FORGE_REAPER_INTERVAL", defaultReaperInterval)
	if err != nil {
		return err
	}
	reaperBatch, err := positiveIntFromEnv("FORGE_REAPER_BATCH_SIZE", defaultReaperBatch)
	if err != nil {
		return err
	}
	recoveryBackoff, err := durationFromEnv("FORGE_RECOVERY_BACKOFF", defaultRecoveryBackoff)
	if err != nil {
		return err
	}
	retryBase, err := durationFromEnv("FORGE_RETRY_BASE_DELAY", defaultRetryBaseDelay)
	if err != nil {
		return err
	}
	retryMax, err := durationFromEnv("FORGE_RETRY_MAX_DELAY", defaultRetryMaxDelay)
	if err != nil {
		return err
	}

	// Retention is off unless the operator sets a time: finished jobs are kept forever.
	retentionSucceeded, err := optionalDurationFromEnv("FORGE_RETENTION_SUCCEEDED")
	if err != nil {
		return err
	}
	retentionFailed, err := optionalDurationFromEnv("FORGE_RETENTION_FAILED")
	if err != nil {
		return err
	}
	retentionInterval, err := durationFromEnv("FORGE_RETENTION_INTERVAL", defaultRetentionInterval)
	if err != nil {
		return err
	}

	// Open PostgreSQL first: without it Forge cannot store jobs, and there is
	// deliberately no fallback to a different store. The deferred Close is
	// registered before the reaper's cleanup below, so (defers run last-in
	// first-out) it runs after the reaper has stopped using the database.
	gormDB, sqlDB, err := openDatabase(os.Getenv("FORGE_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer func() {
		if err := sqlDB.Close(); err != nil {
			log.Printf("forge: close database: %v", err)
		} else {
			log.Print("forge: database connection closed")
		}
	}()
	jobStore, err := store.NewPostgresStore(gormDB)
	if err != nil {
		return fmt.Errorf("create job store: %w", err)
	}

	// The lease and retry settings are validated here: remote workers are
	// handed these values, so unsafe ones must not start.
	if err := worker.ValidateLeaseTiming(leaseDuration, heartbeat, safetyMargin); err != nil {
		return fmt.Errorf("lease settings: %w", err)
	}
	if err := worker.ValidateRetryDelays(retryBase, retryMax); err != nil {
		return fmt.Errorf("retry settings: %w", err)
	}
	m := metrics.New(remoteTypes) // counters for this process; the gauges are added below
	reaper, err := worker.NewReaper(jobStore, reaperInterval, reaperBatch, recoveryBackoff)
	if err != nil {
		return fmt.Errorf("create reaper: %w", err)
	}
	reaper.UseMetrics(m)
	var retention *worker.Retention
	if retentionSucceeded > 0 || retentionFailed > 0 {
		retention, err = worker.NewRetention(jobStore, retentionInterval, retentionSucceeded, retentionFailed)
		if err != nil {
			return fmt.Errorf("retention settings: %w", err)
		}
		retention.UseMetrics(m)
	}
	service, err := worker.NewService(jobStore, remoteTypes, maxQueued, maxJobTimeout)
	if err != nil {
		return fmt.Errorf("create job service: %w", err)
	}
	service.UseMetrics(m)
	if typePolicy != nil {
		service.UsePolicy(typePolicy.Types)
	}
	workerAPI, err := workerapi.NewServer(workerapi.Config{
		Store:        jobStore,
		AllowedTypes: remoteTypes,
		Keys:         workerKeys,
		Lease:        leaseDuration,
		Heartbeat:    heartbeat,
		Safety:       safetyMargin,
		Deadline:     executionTimeout,
		Metrics:      m,
		RetryBase:    retryBase,
		RetryMax:     retryMax,
		PollInterval: pollInterval,
		MaxPolls:     workerMaxPoll,
	})
	if err != nil {
		return fmt.Errorf("create worker api: %w", err)
	}
	m.RegisterQueue(jobStore, maxQueued, service.ConfiguredTypes())
	m.RegisterDB(sqlDB)
	m.RegisterAdmission(store.AdmissionSlots)
	apiServer, err := api.NewServer(jobStore, service, submitKeys)
	if err != nil {
		return fmt.Errorf("create api: %w", err)
	}

	var operatorAPI *operatorapi.Server
	if operatorAddr != "" {
		operatorAPI, err = operatorapi.NewServer(operatorapi.Config{
			Stats:     jobStore,
			Admission: service,
			Keys:      operatorKeys,
			Metrics:   m,
		})
		if err != nil {
			return fmt.Errorf("create operator api: %w", err)
		}
	}

	// Jobs queued in PostgreSQL before this start are claimed by workers
	// without being submitted again; say how many there are.
	startCtx, cancelStart := context.WithTimeout(context.Background(), dbStartupTimeout)
	waiting, err := jobStore.CountQueued(startCtx)
	cancelStart()
	if err != nil {
		return fmt.Errorf("count queued jobs: %w", err)
	}

	// Start the reaper (it requeues jobs whose worker stopped renewing the
	// lease). This deferred cleanup runs on every path out of run, including an
	// HTTP server failure.
	reaperCtx, cancelReaper := context.WithCancel(context.Background())
	reaper.Start(reaperCtx)
	defer func() {
		cancelReaper()
		if reaper.WaitFor(workerDrainTimeout) {
			log.Print("forge: reaper stopped")
			return
		}
		log.Printf("forge: reaper did not stop within %s", workerDrainTimeout)
		if runErr == nil {
			runErr = errors.New("shutdown incomplete: reaper still running")
		}
	}()

	if retention != nil {
		retentionCtx, cancelRetention := context.WithCancel(context.Background())
		retention.Start(retentionCtx)
		defer func() {
			cancelRetention()
			if retention.WaitFor(workerDrainTimeout) {
				log.Print("forge: retention stopped")
				return
			}
			log.Printf("forge: retention did not stop within %s", workerDrainTimeout)
			if runErr == nil {
				runErr = errors.New("shutdown incomplete: retention still running")
			}
		}()
		log.Printf("forge: retention: succeeded jobs kept %s, failed jobs kept %s (0s = forever); checked every %s", retentionSucceeded, retentionFailed, retentionInterval)
	} else {
		log.Print("forge: retention is off: finished jobs are never deleted (set FORGE_RETENTION_SUCCEEDED and/or FORGE_RETENTION_FAILED to change that)")
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: apiServer.Handler(),
		// Jobs run in the background, so every request should be quick; these
		// limits stop slow or stalled clients from holding connections open.
		ReadHeaderTimeout: 5 * time.Second,  // time to send the request headers
		ReadTimeout:       15 * time.Second, // time to send the whole request (max body is 1 MB)
		WriteTimeout:      15 * time.Second, // time to send the response
		IdleTimeout:       60 * time.Second, // how long a keep-alive connection may sit unused
	}

	// Ctrl-C (SIGINT) or SIGTERM cancels sigCtx.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// ListenAndServe blocks, so it runs in its own goroutine. The channel is
	// buffered so that goroutine can always finish its send and exit.
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- httpServer.ListenAndServe()
	}()

	// The worker API has its own server.
	workerServer := &http.Server{
		Addr:              workerAddr,
		Handler:           workerAPI.Handler(),
		ReadHeaderTimeout: workerapi.ReadHeaderTimeout,
		ReadTimeout:       workerapi.ReadTimeout,
		WriteTimeout:      workerapi.WriteTimeout, // longer than the 25 s claim wait
		IdleTimeout:       workerapi.IdleTimeout,
	}
	defer workerServer.Close() // covers early returns; a no-op after a clean Shutdown
	workerServerErr := make(chan error, 1)
	go func() {
		workerServerErr <- workerServer.ListenAndServe()
	}()
	// The operator API has its own server too (nil channel = not configured).
	var operatorServerErr chan error
	var operatorServer *http.Server
	if operatorAPI != nil {
		operatorServer = &http.Server{
			Addr:              operatorAddr,
			Handler:           operatorAPI.Handler(),
			ReadHeaderTimeout: operatorapi.ReadHeaderTimeout,
			ReadTimeout:       operatorapi.ReadTimeout,
			WriteTimeout:      operatorapi.WriteTimeout,
			IdleTimeout:       operatorapi.IdleTimeout,
		}
		defer operatorServer.Close()
		operatorServerErr = make(chan error, 1)
		go func() {
			operatorServerErr <- operatorServer.ListenAndServe()
		}()
		log.Printf("forge: operator API listening on %s (read-only GET /v1/queues; bearer key)", operatorAddr)
		if !isLoopbackAddr(operatorAddr) {
			log.Printf("forge: WARNING: the operator API address %s is not loopback; the key travels in clear text, and the endpoint reveals queue activity", operatorAddr)
		}
	}
	log.Printf("forge: listening on %s (max queued %d); allowed job types: %s; %d job(s) already queued in the database",
		addr, maxQueued, strings.Join(remoteTypes, ","), waiting)
	log.Printf("forge: worker API listening on %s (claim, renew, complete, fail and release endpoints)", workerAddr)
	log.Print("forge: every listener speaks plain HTTP and the bearer keys travel in clear text; keep Forge on loopback or a trusted private network")
	log.Printf("forge: keys by name (never the keys): submit %v, worker %v, operator %v", submitKeys.Names(), workerKeys.Names(), operatorKeys.Names())
	log.Print("forge: a worker that stops renewing without releasing loses its job when the lease expires; the reaper recovers it, consuming an attempt")
	log.Printf("forge: lease %s, heartbeat %s, safety margin %s; reaper every %s (batch %d, recovery backoff %s); retry backoff %s to %s",
		leaseDuration, heartbeat, safetyMargin, reaperInterval, reaperBatch, recoveryBackoff, retryBase, retryMax)

	select {
	case err := <-serverErr:
		// ListenAndServe only returns on failure here (e.g. port in use).
		return fmt.Errorf("http server: %w", err)
	case err := <-workerServerErr:
		return fmt.Errorf("worker http server: %w", err)
	case err := <-operatorServerErr:
		return fmt.Errorf("operator http server: %w", err)
	case <-sigCtx.Done():
		log.Print("forge: shutdown signal received")
	}

	// Stop accepting requests and give in-flight ones a bounded time to finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if operatorServer != nil {
		if err := operatorServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("operator http shutdown: %w", err)
		}
		log.Print("forge: operator api stopped")
	}
	// Waiting claims answer "no work" at once; otherwise Shutdown would wait
	// for them to time out on their own.
	workerAPI.BeginShutdown()
	if err := workerServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("worker http shutdown: %w", err)
	}
	log.Print("forge: worker api stopped")
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	log.Print("forge: http server stopped")
	return nil
}

// positiveIntFromEnv reads a whole number of at least 1 from the environment,
// or returns def if the variable is unset.
func positiveIntFromEnv(name string, def int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return def, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a whole number of at least 1", name)
	}
	return n, nil
}

// durationFromEnv reads a positive duration such as "500ms" or "2s" from the
// environment, or returns def if the variable is unset.
func durationFromEnv(name string, def time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return def, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as \"1s\"", name)
	}
	return d, nil
}

// optionalDurationFromEnv reads a duration that may be left unset (0). A value must
// parse; whether it is long enough is checked where it is used.
func optionalDurationFromEnv(name string) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as \"168h\" (leave it unset to keep jobs forever)", name)
	}
	return d, nil
}

// openDatabase connects GORM to PostgreSQL and checks the connection within
// dbStartupTimeout. It returns both handles: GORM for the store, and the
// underlying *sql.DB (the connection pool) so main can close it at shutdown.
// The URL is never printed, because it contains the password.
func openDatabase(url string) (*gorm.DB, *sql.DB, error) {
	if url == "" {
		return nil, nil, errors.New("FORGE_DATABASE_URL is not set; copy .env.example to .env and load it " +
			"(see docs/reference.md, \"Running Forge with PostgreSQL\")")
	}
	gormDB, err := gorm.Open(postgres.Open(url), &gorm.Config{
		// Open is lazy about connecting; the explicit ping below does it with a timeout.
		DisableAutomaticPing: true,
		// GORM's default logger prints SQL with parameter values, which would put
		// job input in the logs. Forge logs its own store errors instead.
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open database (check the FORGE_DATABASE_URL format): %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("get database pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(dbMaxConns)
	sqlDB.SetMaxIdleConns(dbMaxConns)

	ctx, cancel := context.WithTimeout(context.Background(), dbStartupTimeout)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("cannot reach PostgreSQL (is it running? try \"docker compose up -d --wait\"; "+
			"check FORGE_DATABASE_URL): %w", err)
	}
	return gormDB, sqlDB, nil
}

// operatorFromEnv reads the operator API address. No address and no operator keys means the
// operator API is off. One without the other is an error, never a silent half-configuration. The
// address must not be one of the other listeners' addresses.
func operatorFromEnv(addr, workerAddr string, haveKeys bool) (opAddr string, err error) {
	rawAddr, addrSet := os.LookupEnv("FORGE_OPERATOR_ADDR")
	if !addrSet && !haveKeys {
		return "", nil
	}
	if addrSet != haveKeys {
		return "", errOperatorHalf
	}
	opAddr = strings.TrimSpace(rawAddr)
	if _, _, err := net.SplitHostPort(opAddr); err != nil {
		return "", fmt.Errorf("FORGE_OPERATOR_ADDR must look like host:port (for example 127.0.0.1:8082): %w", err)
	}
	switch {
	case opAddr == addr:
		return "", errors.New("FORGE_OPERATOR_ADDR must differ from FORGE_ADDR: the operator API has its own listener")
	case workerAddr != "" && opAddr == workerAddr:
		return "", errors.New("FORGE_OPERATOR_ADDR must differ from FORGE_WORKER_ADDR: the operator API has its own listener")
	}
	return opAddr, nil
}

// isLoopbackAddr reports whether a host:port address binds only the local
// machine. An empty host or an unspecified address (0.0.0.0, ::) listens on
// every interface and is not loopback.
func isLoopbackAddr(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
