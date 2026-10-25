package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/Venkatraman-web/forge/internal/auth"
)

// keysFromEnv builds the key set of each API. A role's keys come from the key file
// (FORGE_KEYS_FILE, hashed keys with names, several per role so a key can be rotated) or from its
// single-key variable (FORGE_API_KEY, FORGE_WORKER_API_KEY, FORGE_OPERATOR_API_KEY), never from
// both: two places for one role would make it unclear which keys are live. The submit and worker
// roles are required; the operator role is optional (it goes with FORGE_OPERATOR_ADDR). No key may
// serve two roles, however it was supplied.
func keysFromEnv() (submit, worker, operator auth.KeySet, err error) {
	var file map[string]auth.KeySet
	if path := os.Getenv("FORGE_KEYS_FILE"); path != "" {
		if file, err = auth.LoadKeyFile(path); err != nil {
			return submit, worker, operator, fmt.Errorf("FORGE_KEYS_FILE: %w", err)
		}
	}
	pick := func(role, envName string, required bool) (auth.KeySet, error) {
		plain := os.Getenv(envName)
		fromFile, inFile := file[role]
		switch {
		case inFile && plain != "":
			return auth.KeySet{}, fmt.Errorf("the %s keys are given twice: in FORGE_KEYS_FILE and in %s (use one)", role, envName)
		case inFile:
			return fromFile, nil
		case plain != "":
			return auth.SingleKey(envName, plain)
		case required:
			return auth.KeySet{}, fmt.Errorf("%s is required (or list %q keys in FORGE_KEYS_FILE)", envName, role)
		}
		return auth.KeySet{}, nil
	}
	if submit, err = pick(auth.RoleSubmit, "FORGE_API_KEY", true); err != nil {
		return
	}
	if worker, err = pick(auth.RoleWorker, "FORGE_WORKER_API_KEY", true); err != nil {
		return
	}
	if operator, err = pick(auth.RoleOperator, "FORGE_OPERATOR_API_KEY", false); err != nil {
		return
	}
	err = auth.Distinct(map[string]auth.KeySet{auth.RoleSubmit: submit, auth.RoleWorker: worker, auth.RoleOperator: operator})
	if err != nil {
		err = fmt.Errorf("%w (submitters, workers and operators must not share a credential)", err)
	}
	return
}

var errOperatorHalf = errors.New("FORGE_OPERATOR_ADDR and the operator keys (FORGE_OPERATOR_API_KEY, or \"operator\" in FORGE_KEYS_FILE) must be set together (or neither, to turn the operator API off)")
