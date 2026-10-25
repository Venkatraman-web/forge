package reconcile

import (
	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

// Handler adapts a Reconciler to the worker runtime (see bizjob.Handler for the
// permanent-versus-retryable classification).
func Handler(r *Reconciler) workerruntime.HandlerFunc { return bizjob.Handler(r.Reconcile) }
