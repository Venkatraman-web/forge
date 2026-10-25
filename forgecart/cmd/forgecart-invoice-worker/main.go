// Command forgecart-invoice-worker runs ForgeCart's invoice.generate job
// handler against Forge's worker API. It is separate from the ForgeCart API,
// the outbox dispatcher and the other workers; run as many replicas as you like.
package main

import (
	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/forgecart/internal/invoice"
	"github.com/Venkatraman-web/forge/forgecart/internal/workerapp"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

func main() {
	workerapp.Main("forgecart-invoice-worker", "forgecart-invoice", invoice.EventType,
		func(dbs bizjob.Databases) workerruntime.HandlerFunc { return invoice.Handler(invoice.New(dbs)) })
}
