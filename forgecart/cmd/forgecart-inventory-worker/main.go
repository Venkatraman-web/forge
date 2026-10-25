// Command forgecart-inventory-worker runs ForgeCart's inventory.reconcile job
// handler against Forge's worker API. It is separate from the ForgeCart API and
// the outbox dispatcher; run as many replicas as you like.
package main

import (
	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/forgecart/internal/reconcile"
	"github.com/Venkatraman-web/forge/forgecart/internal/workerapp"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

func main() {
	workerapp.Main("forgecart-inventory-worker", "forgecart-inventory", reconcile.EventType,
		func(dbs bizjob.Databases) workerruntime.HandlerFunc { return reconcile.Handler(reconcile.New(dbs)) })
}
