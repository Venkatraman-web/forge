// Command forgecart-notification-worker runs ForgeCart's notification.send job
// handler against Forge's worker API. Delivery is SIMULATED: it records a
// receipt in ForgeCart's database and contacts no email provider. It is
// separate from the API, the dispatcher and the other workers.
package main

import (
	"github.com/Venkatraman-web/forge/forgecart/internal/bizjob"
	"github.com/Venkatraman-web/forge/forgecart/internal/notification"
	"github.com/Venkatraman-web/forge/forgecart/internal/workerapp"
	"github.com/Venkatraman-web/forge/sdk/workerruntime"
)

func main() {
	workerapp.Main("forgecart-notification-worker", "forgecart-notification", notification.EventType,
		func(dbs bizjob.Databases) workerruntime.HandlerFunc {
			return notification.Handler(notification.New(dbs))
		})
}
