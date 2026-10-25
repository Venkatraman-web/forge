// Package policy loads Forge's optional server configuration file: which job
// types exist and, per type, the default and the maximum retry budget and
// execution timeout. Without the file Forge behaves as before (job types from
// FORGE_REMOTE_JOB_TYPES, defaults of 3 attempts and 30 s).
//
// The file is JSON, decoded strictly (unknown keys are errors, so a misspelled
// setting never silently does nothing) and read once at startup; a change
// takes effect on restart.
//
//	{
//	  "version": 1,
//	  "job_types": {
//	    "invoice.generate": {
//	      "default_max_attempts": 5, "max_max_attempts": 8,
//	      "default_timeout": "15s",  "max_timeout": "60s"
//	    }
//	  }
//	}
//
// All four fields are optional. A submitter's own options.max_attempts and
// options.timeout_seconds still win over a default, but cannot exceed the
// type's maximum.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/Venkatraman-web/forge/internal/jobs"
	"github.com/Venkatraman-web/forge/internal/worker"
)

const (
	// Version is the only file layout this server understands.
	Version = 1
	// maxFileBytes bounds the file: it is operator-written, but still read whole.
	maxFileBytes = 1 << 20
)

// Policy is the loaded file.
type Policy struct {
	Types map[string]jobs.TypePolicy `json:"job_types"`
}

type fileType struct {
	DefaultMaxAttempts int    `json:"default_max_attempts"`
	MaxMaxAttempts     int    `json:"max_max_attempts"`
	DefaultTimeout     string `json:"default_timeout"`
	MaxTimeout         string `json:"max_timeout"`
}

type file struct {
	Version  int                 `json:"version"`
	JobTypes map[string]fileType `json:"job_types"`
}

// Load reads and validates the file. maxJobTimeout is the server-wide cap
// (FORGE_MAX_JOB_TIMEOUT): a type may not promise more than the server allows.
func Load(path string, maxJobTimeout time.Duration) (*Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	if len(raw) > maxFileBytes {
		return nil, fmt.Errorf("config file is larger than %d bytes", maxFileBytes)
	}
	return parse(raw, maxJobTimeout)
}

func parse(raw []byte, maxJobTimeout time.Duration) (*Policy, error) {
	var in file
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	if dec.More() {
		return nil, errors.New("config file: unexpected data after the JSON document")
	}
	if in.Version != Version {
		return nil, fmt.Errorf("config file: version must be %d, got %d", Version, in.Version)
	}
	if len(in.JobTypes) == 0 {
		return nil, errors.New("config file: job_types must list at least one job type")
	}
	names := make([]string, 0, len(in.JobTypes))
	for name := range in.JobTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	// The same name rules as FORGE_REMOTE_JOB_TYPES, including the count limit.
	if _, err := worker.ParseRemoteJobTypes(joinNames(names)); err != nil {
		return nil, fmt.Errorf("config file: job_types: %w", err)
	}

	p := &Policy{Types: make(map[string]jobs.TypePolicy, len(names))}
	for _, name := range names {
		t, err := resolve(in.JobTypes[name], maxJobTimeout)
		if err != nil {
			return nil, fmt.Errorf("config file: job type %q: %w", name, err)
		}
		p.Types[name] = t
	}
	return p, nil
}

func resolve(in fileType, maxJobTimeout time.Duration) (jobs.TypePolicy, error) {
	t := jobs.TypePolicy{DefaultMaxAttempts: in.DefaultMaxAttempts, MaxMaxAttempts: in.MaxMaxAttempts}
	for _, a := range []struct {
		name string
		v    int
	}{{"default_max_attempts", t.DefaultMaxAttempts}, {"max_max_attempts", t.MaxMaxAttempts}} {
		if a.v < 0 || a.v > jobs.MaxAttemptsLimit {
			return t, fmt.Errorf("%s must be from 1 to %d", a.name, jobs.MaxAttemptsLimit)
		}
	}
	if t.DefaultMaxAttempts > 0 && t.MaxMaxAttempts > 0 && t.DefaultMaxAttempts > t.MaxMaxAttempts {
		return t, errors.New("default_max_attempts is larger than max_max_attempts")
	}
	var err error
	if t.DefaultTimeoutSecs, err = seconds("default_timeout", in.DefaultTimeout, maxJobTimeout); err != nil {
		return t, err
	}
	if t.MaxTimeoutSecs, err = seconds("max_timeout", in.MaxTimeout, maxJobTimeout); err != nil {
		return t, err
	}
	if t.DefaultTimeoutSecs > 0 && t.MaxTimeoutSecs > 0 && t.DefaultTimeoutSecs > t.MaxTimeoutSecs {
		return t, errors.New("default_timeout is larger than max_timeout")
	}
	return t, nil
}

// seconds parses a duration such as "15s" that must be a whole number of seconds
// within the server-wide cap. Empty means not set.
func seconds(name, value string, maxJobTimeout time.Duration) (int, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	switch {
	case err != nil:
		return 0, fmt.Errorf("%s must be a duration such as \"15s\"", name)
	case d < time.Second || d%time.Second != 0:
		return 0, fmt.Errorf("%s must be a whole number of seconds, at least 1s", name)
	case d > maxJobTimeout:
		return 0, fmt.Errorf("%s %s is above the server maximum %s (FORGE_MAX_JOB_TIMEOUT)", name, d, maxJobTimeout)
	}
	return int(d / time.Second), nil
}

// TypeNames lists the declared job types, sorted.
func (p *Policy) TypeNames() []string {
	names := make([]string, 0, len(p.Types))
	for n := range p.Types {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func joinNames(names []string) string {
	b := &bytes.Buffer{}
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
	}
	return b.String()
}
