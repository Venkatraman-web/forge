// Command forgecart-order-worker runs ForgeCart's order.process job handler
// against Forge's worker API: the invoice, the fulfillment plan and the
// confirmation notification of a paid order, in one job. It is separate from the
// ForgeCart API, the outbox dispatcher and the other workers; run as many
// replicas as you like.
package main

import (
	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/forgecart/internal/postpay"
	"github.com/Venkatraman-web/forge/forgecart/internal/workerapp"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

func main() {
	workerapp.Main("forgecart-order-worker", "forgecart-order", postpay.EventType,
		func(dbs bizjob.Databases) workerruntime.HandlerFunc { return postpay.Handler(postpay.New(dbs)) })
}
