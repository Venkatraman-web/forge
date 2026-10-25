// Command forgecart-fulfillment-worker runs ForgeCart's fulfillment.prepare job
// handler against Forge's worker API. Fulfillment is SIMULATED: it records a
// plan in ForgeCart's database and ships nothing. It is separate from the API,
// the dispatcher and the other workers.
package main

import (
	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/forgecart/internal/fulfillment"
	"github.com/Venkatraman-web/forge/forgecart/internal/workerapp"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

func main() {
	workerapp.Main("forgecart-fulfillment-worker", "forgecart-fulfillment", fulfillment.EventType,
		func(dbs bizjob.Databases) workerruntime.HandlerFunc { return fulfillment.Handler(fulfillment.New(dbs)) })
}
