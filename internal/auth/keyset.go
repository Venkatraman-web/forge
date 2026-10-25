package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// A KeySet is the group of keys that may use one API. Holding several at once is what makes a key
// rotation possible without downtime: add the new key, move the clients over, then remove the old
// one. Each key has a name, so a log line can say WHICH key acted without ever containing the key.
// Only SHA-256 digests are kept.
type KeySet struct{ entries []entry }

type entry struct {
	name string
	sum  [sha256.Size]byte
}

// MaxKeysPerRole bounds the file, so a typo cannot turn every request into a long scan.
const MaxKeysPerRole = 32

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,39}$`)

// SingleKey is a KeySet of one key named "default", from a plain-text key (the FORGE_*_API_KEY
// variables). what names the key in the error message ("worker API key").
func SingleKey(what, plain string) (KeySet, error) {
	k, err := NewKey(what, plain)
	if err != nil {
		return KeySet{}, err
	}
	return KeySet{entries: []entry{{name: "default", sum: k.sum}}}, nil
}

// Empty reports whether the set holds no key (nothing could ever authenticate).
func (ks KeySet) Empty() bool { return len(ks.entries) == 0 }

// Names lists the key names, in file order. It is safe to log.
func (ks KeySet) Names() []string {
	out := make([]string, len(ks.entries))
	for i, e := range ks.entries {
		out[i] = e.name
	}
	return out
}

// Match reports which key an Authorization header carries. Every key is compared, in constant
// time, whether or not an earlier one matched, so the time taken reveals neither which key was
// close nor how many there are. A missing header, another scheme, an empty token and a wrong key
// are indistinguishable.
func (ks KeySet) Match(header string) (name string, ok bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	presented := sha256.Sum256([]byte(header[len(prefix):]))
	for _, e := range ks.entries {
		if subtle.ConstantTimeCompare(presented[:], e.sum[:]) == 1 {
			name, ok = e.name, true
		}
	}
	return name, ok
}

type nameKey struct{}

// WithName returns a context that remembers which key authenticated the request.
func WithName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, nameKey{}, name)
}

// NameFrom returns the name stored by WithName, or "" when there is none.
func NameFrom(ctx context.Context) string {
	n, _ := ctx.Value(nameKey{}).(string)
	return n
}

// Role names in the key file.
const (
	RoleSubmit   = "submit"
	RoleWorker   = "worker"
	RoleOperator = "operator"
)

// maxKeyFileBytes is far more than 3 roles of 32 keys need.
const maxKeyFileBytes = 64 << 10

type fileEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type keyFile struct {
	Submit   []fileEntry `json:"submit"`
	Worker   []fileEntry `json:"worker"`
	Operator []fileEntry `json:"operator"`
}

// LoadKeyFile reads a key file: JSON with optional "submit", "worker" and "operator" lists of
// {"name": ..., "sha256": ...}, where sha256 is the lower-case hex digest of the key (so the file
// holds no secret that works on its own). A role the file does not mention is absent from the
// result. The file is rejected if it has an unknown field, a bad name or digest, a repeated name
// in one role, an empty list, or the same digest anywhere twice (one key must never serve two roles).
//
// A digest says nothing about how long or random the original key was; generate keys with
// something like `openssl rand -hex 24` and hash them with `printf %s KEY | sha256sum`.
func LoadKeyFile(path string) (map[string]KeySet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	if len(raw) > maxKeyFileBytes {
		return nil, fmt.Errorf("key file is larger than %d bytes", maxKeyFileBytes)
	}
	return parseKeyFile(raw)
}

func parseKeyFile(raw []byte) (map[string]KeySet, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var kf keyFile
	if err := dec.Decode(&kf); err != nil {
		return nil, fmt.Errorf("key file is not valid (expected only \"submit\", \"worker\" and \"operator\" lists): %w", err)
	}
	if dec.More() {
		return nil, errors.New("key file has data after the JSON object")
	}
	out := map[string]KeySet{}
	seen := map[[sha256.Size]byte]string{}
	for _, role := range []struct {
		name    string
		entries []fileEntry
	}{{RoleSubmit, kf.Submit}, {RoleWorker, kf.Worker}, {RoleOperator, kf.Operator}} {
		if role.entries == nil {
			continue
		}
		if len(role.entries) == 0 || len(role.entries) > MaxKeysPerRole {
			return nil, fmt.Errorf("key file: %q must list 1 to %d keys (leave the role out to configure it another way)", role.name, MaxKeysPerRole)
		}
		var ks KeySet
		names := map[string]bool{}
		for i, e := range role.entries {
			switch {
			case !namePattern.MatchString(e.Name):
				return nil, fmt.Errorf("key file: %s[%d] name must be 1 to 40 characters from a-z 0-9 . _ - starting with a letter", role.name, i)
			case names[e.Name]:
				return nil, fmt.Errorf("key file: %s has the name %q twice", role.name, e.Name)
			}
			b, err := hex.DecodeString(e.SHA256)
			if err != nil || len(b) != sha256.Size || e.SHA256 != strings.ToLower(e.SHA256) {
				return nil, fmt.Errorf("key file: %s[%d] (%q) sha256 must be 64 lower-case hex characters", role.name, i, e.Name)
			}
			var sum [sha256.Size]byte
			copy(sum[:], b)
			if prev, dup := seen[sum]; dup {
				return nil, fmt.Errorf("key file: %s %q has the same key as %s: one key must not be listed twice", role.name, e.Name, prev)
			}
			seen[sum] = role.name + " " + fmt.Sprintf("%q", e.Name)
			names[e.Name] = true
			ks.entries = append(ks.entries, entry{name: e.Name, sum: sum})
		}
		out[role.name] = ks
	}
	return out, nil
}

// Distinct returns an error if any two of the given roles share a key. It is what keeps a
// submitter's key from also working as a worker or operator key, however the keys were supplied.
func Distinct(roles map[string]KeySet) error {
	seen := map[[sha256.Size]byte]string{}
	for _, role := range []string{RoleSubmit, RoleWorker, RoleOperator} {
		for _, e := range roles[role].entries {
			if prev, dup := seen[e.sum]; dup && prev != role {
				return fmt.Errorf("the %s and %s roles must not share a key (%s key %q)", prev, role, role, e.name)
			}
			seen[e.sum] = role
		}
	}
	return nil
}
