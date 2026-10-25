package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Persistence operations for a future dispatcher. Nothing in this package
// calls them yet and nothing sends anything to Forge.
//
// Every state change is one conditional UPDATE keyed on the event ID, the
// claim token and the database clock, and zero matching rows means the caller
// no longer owns the event (ErrLeaseLost). The token is what fences a stale
// owner: a reclaim replaces it, so an old holder's token matches nothing.

const (
	MinBatch = 1
	MaxBatch = 100
	// MaxLease is a sanity bound, not a recommendation; a lease should be a
	// little longer than the publisher's request deadline.
	MaxLease = time.Hour
	// MaxRetryDelay bounds how far ahead a retry may be scheduled (the delay is
	// applied by PostgreSQL's clock).
	MaxRetryDelay = 30 * 24 * time.Hour
)

var (
	// ErrLeaseLost means the event is not pending, the token does not match its
	// current claim, or the lease has expired. Nothing was changed.
	ErrLeaseLost = errors.New("outbox: lease lost or invalid")
	// ErrInvalidInput means the arguments were rejected before touching the database.
	ErrInvalidInput = errors.New("outbox: invalid input")
)

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Event is a claimed outbox event. ClaimToken is the secret proof of ownership
// for this claim only; it is never serialized and must not be logged.
type Event struct {
	ID       string          `json:"id"`
	OrderID  string          `json:"order_id"`
	Type     string          `json:"event_type"`
	Payload  json.RawMessage `json:"payload"`
	Attempts int             `json:"attempts"`
	// UnchargedAttempts of those attempts ended for reasons unrelated to the
	// event (see RescheduleUncharged); Attempts - UnchargedAttempts is the
	// retry budget already used.
	UnchargedAttempts int       `json:"uncharged_attempts"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
	ClaimToken        string    `json:"-"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Claim leases up to batch due events and returns them, oldest first
// (next_attempt_at, then id). An event is eligible when it is pending, due by
// the database clock, and has no lease or an expired one (a crashed holder).
//
// One statement does the work: the inner SELECT ... FOR UPDATE SKIP LOCKED
// locks candidate rows and skips rows another dispatcher has locked right now,
// and the UPDATE assigns each locked row its OWN fresh random token (128 bits
// from crypto/rand) and a lease expiry computed from database time. Two
// dispatchers can therefore never actively claim the same event, and a
// reclaimed event gets a different token even if the same process reclaims it.
func (s *Store) Claim(ctx context.Context, batch int, lease time.Duration) ([]Event, error) {
	if batch < MinBatch || batch > MaxBatch {
		return nil, fmt.Errorf("%w: batch must be %d to %d", ErrInvalidInput, MinBatch, MaxBatch)
	}
	if lease <= 0 || lease > MaxLease {
		return nil, fmt.Errorf("%w: lease must be positive and at most %s", ErrInvalidInput, MaxLease)
	}
	tokens := make([]string, batch)
	for i := range tokens {
		t, err := newToken()
		if err != nil {
			return nil, err
		}
		tokens[i] = t
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH due AS (
			SELECT id, next_attempt_at FROM outbox_events
			WHERE status = 'pending'
			  AND next_attempt_at <= now()
			  AND (lease_expires_at IS NULL OR lease_expires_at <= now())
			ORDER BY next_attempt_at, id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), numbered AS (
			SELECT id, row_number() OVER (ORDER BY next_attempt_at, id) AS n FROM due
		), fresh AS (
			SELECT t AS token, n FROM unnest($3::text[]) WITH ORDINALITY AS u(t, n)
		)
		UPDATE outbox_events e
		SET leased_by = fresh.token,
		    lease_expires_at = now() + make_interval(secs => $2::float8),
		    updated_at = now()
		FROM numbered JOIN fresh ON fresh.n = numbered.n
		WHERE e.id = numbered.id
		RETURNING e.id, e.order_id, e.event_type, e.payload, e.attempts, e.uncharged_attempts, e.lease_expires_at, e.leased_by, e.next_attempt_at`,
		batch, lease.Seconds(), tokens)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type claimed struct {
		Event
		due time.Time
	}
	var got []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.ID, &c.OrderID, &c.Type, &c.Payload, &c.Attempts, &c.UnchargedAttempts, &c.LeaseExpiresAt, &c.ClaimToken, &c.due); err != nil {
			return nil, err
		}
		c.LeaseExpiresAt = c.LeaseExpiresAt.UTC()
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(got, func(i, j int) bool {
		if !got[i].due.Equal(got[j].due) {
			return got[i].due.Before(got[j].due)
		}
		return got[i].ID < got[j].ID
	})
	events := make([]Event, len(got))
	for i, c := range got {
		events[i] = c.Event
	}
	return events, nil
}

func checkToken(token string) error {
	if !tokenPattern.MatchString(token) {
		return fmt.Errorf("%w: malformed claim token", ErrInvalidInput)
	}
	return nil
}

// exec runs one conditional UPDATE and converts "no row matched" into ErrLeaseLost.
func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Owned names one claimed event and the token that proves this caller owns it.
type Owned struct{ ID, Token string }

// split turns a group into the two parallel arrays a batch statement takes.
func split(group []Owned) (ids, tokens []string, err error) {
	for _, o := range group {
		if err := checkToken(o.Token); err != nil {
			return nil, nil, err
		}
		ids = append(ids, o.ID)
		tokens = append(tokens, o.Token)
	}
	return ids, tokens, nil
}

// StartAttempts records that a publication attempt is about to begin for a whole
// group of events, in ONE statement and one commit, and returns the new attempts
// count. Call it immediately before the Forge request. It counts attempts
// STARTED, not attempts that reached Forge: if the process crashes after this
// commits but before the request is sent, the counter has still been incremented
// (the increment cannot be atomic with an HTTP call). The ownership rule is
// applied to each event separately. It returns the new attempts count of every
// event this caller still owns; an event
// missing from the result lost its lease (the batch form of ErrLeaseLost), and
// the others are unaffected. Done one event at a time, each is its own durable
// commit, which at about 3 ms apiece capped the dispatcher.
func (s *Store) StartAttempts(ctx context.Context, group []Owned) (map[string]int, error) {
	ids, tokens, err := split(group)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		UPDATE outbox_events e SET attempts = e.attempts + 1, updated_at = now()
		FROM unnest($1::text[], $2::text[]) AS v(id, token)
		WHERE e.id = v.id AND e.leased_by = v.token AND e.status = 'pending' AND e.lease_expires_at > now()
		RETURNING e.id, e.attempts`, ids, tokens)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int, len(group))
	for rows.Next() {
		var id string
		var attempts int
		if err := rows.Scan(&id, &attempts); err != nil {
			return nil, err
		}
		out[id] = attempts
	}
	return out, rows.Err()
}

// MarkPublishedBatch is MarkPublished for a whole group in one statement and one
// commit. It returns the IDs it marked; an event missing from the result lost its
// lease and was not changed.
func (s *Store) MarkPublishedBatch(ctx context.Context, group []Owned) (map[string]bool, error) {
	ids, tokens, err := split(group)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		UPDATE outbox_events e
		SET status = 'published', published_at = now(), updated_at = now(),
		    leased_by = NULL, lease_expires_at = NULL
		FROM unnest($1::text[], $2::text[]) AS v(id, token)
		WHERE e.id = v.id AND e.leased_by = v.token AND e.status = 'pending' AND e.lease_expires_at > now()
		RETURNING e.id`, ids, tokens)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool, len(group))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MarkPublished records that the event was handed to Forge and clears the
// lease. "Published" means ONLY that Forge accepted the job submission; it says
// nothing about the job having run or succeeded. It matches only a pending
// event with this token and an unexpired lease, so a published event cannot be
// changed again by these operations.
func (s *Store) MarkPublished(ctx context.Context, id, token string) error {
	if err := checkToken(token); err != nil {
		return err
	}
	return s.exec(ctx, `
		UPDATE outbox_events
		SET status = 'published', published_at = now(), updated_at = now(),
		    leased_by = NULL, lease_expires_at = NULL
		WHERE id = $1 AND leased_by = $2 AND status = 'pending' AND lease_expires_at > now()`, id, token)
}

// RescheduleAfter keeps the event pending, makes it due again after delay
// (counted from the DATABASE clock inside the update, so a skewed application
// clock cannot make an event due too early or too late) and clears the lease.
// reason is a short machine code stored as last_error_code. The event ID is
// unchanged, so every attempt publishes the same stable identity.
func (s *Store) RescheduleAfter(ctx context.Context, id, token string, delay time.Duration, reason string) error {
	return s.reschedule(ctx, id, token, delay, reason, 0)
}

// RescheduleUncharged is RescheduleAfter for an attempt that failed for a reason
// unrelated to the event (a Forge-wide configuration failure, or shutdown): it
// also records the attempt as uncharged, so it does not use up the event's
// retry budget no matter how many times the dispatcher is restarted.
func (s *Store) RescheduleUncharged(ctx context.Context, id, token string, delay time.Duration, reason string) error {
	return s.reschedule(ctx, id, token, delay, reason, 1)
}

func (s *Store) reschedule(ctx context.Context, id, token string, delay time.Duration, reason string, uncharge int) error {
	if err := checkToken(token); err != nil {
		return err
	}
	if delay <= 0 || delay > MaxRetryDelay {
		return fmt.Errorf("%w: retry delay must be positive and at most %s", ErrInvalidInput, MaxRetryDelay)
	}
	if err := checkReason(reason); err != nil {
		return err
	}
	return s.exec(ctx, `
		UPDATE outbox_events
		SET next_attempt_at = now() + make_interval(secs => $3::float8), updated_at = now(),
		    last_error_code = $4, uncharged_attempts = uncharged_attempts + $5,
		    leased_by = NULL, lease_expires_at = NULL
		WHERE id = $1 AND leased_by = $2 AND status = 'pending' AND lease_expires_at > now()`,
		id, token, delay.Seconds(), reason, uncharge)
}

// MarkFailed gives up on the event: status becomes 'failed' (terminal; no
// operation here ever claims or changes it again) with a sanitized reason code,
// and the lease is cleared. Same ownership conditions as the other operations.
func (s *Store) MarkFailed(ctx context.Context, id, token, reason string) error {
	if err := checkToken(token); err != nil {
		return err
	}
	if err := checkReason(reason); err != nil {
		return err
	}
	return s.exec(ctx, `
		UPDATE outbox_events
		SET status = 'failed', failed_at = now(), updated_at = now(), last_error_code = $3,
		    leased_by = NULL, lease_expires_at = NULL
		WHERE id = $1 AND leased_by = $2 AND status = 'pending' AND lease_expires_at > now()`,
		id, token, reason)
}

var reasonPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func checkReason(reason string) error {
	if !reasonPattern.MatchString(reason) {
		return fmt.Errorf("%w: reason must be a short lowercase code", ErrInvalidInput)
	}
	return nil
}
