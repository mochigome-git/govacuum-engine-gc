package mqtt

import (
	"log"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// StartHeartbeat launches a goroutine that publishes a heartbeat message
// to topic every interval until stop is closed, reusing the same EMQX
// publish client (and PublishMetric) that finished computed metrics go
// out on. Lands in the same table as those metrics, with
// status.kind="heartbeat" and everything else null — same shape
// gopub-edge's own heartbeat (and the device-side ESP32 heartbeats)
// already produce, so nothing downstream needs special-casing.
//
// startTime should be captured at process boot so uptime_s reflects how
// long this vacuum-engine process has been running.
func StartHeartbeat(client paho.Client, stop <-chan struct{}, topic, tenantID, deviceID, version string, startTime time.Time, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		log.Printf("[vacuum-engine] ✓ heartbeat started — every %v (tenant_id=%s device_id=%s)", interval, tenantID, deviceID)

		for {
			select {
			case <-stop:
				log.Println("[vacuum-engine] heartbeat stopped")
				return
			case <-ticker.C:
				hb := map[string]any{
					"tenant_id": tenantID,
					"device_id": deviceID,
					"status": map[string]any{
						"kind":     "heartbeat",
						"fw":       version,
						"ts":       time.Now().UTC().Format(time.RFC3339),
						"uptime_s": int64(time.Since(startTime).Seconds()),
					},
				}
				if err := PublishMetric(client, topic, hb); err != nil {
					log.Printf("[vacuum-engine] ⚠ heartbeat publish failed: %v", err)
				} else {
					//log.Printf("[vacuum-engine] ♥ heartbeat published (topic=%s uptime_s=%v)", topic, hb["status"].(map[string]any)["uptime_s"])
				}
			}
		}
	}()
}
