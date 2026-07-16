package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"govacuum-engine-gc/internal/config"
	"govacuum-engine-gc/internal/db"
	"govacuum-engine-gc/internal/logic"
	"govacuum-engine-gc/internal/mqtt"
)

// incomingRequest is the flat envelope gopub-edge publishes locally — see
// its mqttpub package (PublishAndAwaitReplyLocal). Only the fields this
// engine cares about are declared; anything else in the JSON is ignored by
// json.Unmarshal.
type incomingRequest struct {
	TenantID   string          `json:"tenant_id"`
	DeviceID   string          `json:"device_id"`
	ReplyTopic string          `json:"reply_topic"`
	Readings   json.RawMessage `json:"readings"`
}

// vacuumReadings is what we expect inside "readings" for a vacuum request.
type vacuumReadings struct {
	VacuumStart     *float64 `json:"vacuum_start"`
	VacuumLeave1min *float64 `json:"vacuum_leave_1min"`
	VacuumLeave2min *float64 `json:"vacuum_leave_2min"`
	VacuumLeave3min *float64 `json:"vacuum_leave_3min"`
}

// localReplyData mirrors gopub-edge's patch.VacuumData exactly — this is
// what gets marshaled into the LOCAL reply's "data" field, so
// SendUpsertRequest's parsing (and PLC write-back) works unchanged.
// XStatus/YStatus are the descriptive classification strings ("Within
// IQR", "Outlier", "Building Data", "No Data", ...) — gopub-edge writes
// these to W10/W20, which are word/text registers expecting a status
// label, not boolean coils. Only VacuumStatus is a real bool (M4350).
type localReplyData struct {
	ID              string   `json:"id"`
	CreatedAt       string   `json:"created_at"`
	VacuumStart     int      `json:"vacuum_start"`
	VacuumLeave1min float64  `json:"vacuum_leave_1min"`
	VacuumLeave2min float64  `json:"vacuum_leave_2min"`
	VacuumLeave3min float64  `json:"vacuum_leave_3min"`
	XStatus         string   `json:"x_status"`
	YStatus         string   `json:"y_status"`
	VacuumStatus    bool     `json:"vacuum_status"`
	X               *float64 `json:"x"`
	Y               *float64 `json:"y"`
}

func main() {
	startTime := time.Now()
	cfg := config.Load()

	// EMQX — publish-only, connect before Local so the handler closure
	// below can always reach it once a request actually arrives.
	emqxClient, err := mqtt.ConnectEMQXPublisher(cfg.EMQXPublish)
	if err != nil {
		log.Fatalf("failed to connect to EMQX: %v", err)
	}
	defer emqxClient.Disconnect(250)

	stopHeartbeat := make(chan struct{})
	mqtt.StartHeartbeat(emqxClient, stopHeartbeat, cfg.EMQXPublish.RequestTopic, cfg.TenantID, cfg.DeviceID, cfg.AppVersion, startTime, cfg.HeartbeatInterval)

	// localClient is assigned below, but the handler closure captures the
	// variable (not its value at closure-creation time) — safe because
	// paho won't invoke the handler until Subscribe (inside ConnectLocal)
	// completes, by which point localClient is already assigned.
	var localClient paho.Client
	handler := func(_ paho.Client, msg paho.Message) {
		handleRequest(context.Background(), localClient, emqxClient, cfg, msg.Payload())
	}

	localClient, err = mqtt.ConnectLocal(cfg.Local, handler)
	if err != nil {
		log.Fatalf("failed to connect to local Mosquitto: %v", err)
	}
	defer localClient.Disconnect(250)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	close(stopHeartbeat)
	log.Println("vacuum-engine shutting down")
}

// handleRequest parses one incoming local request, ignores anything that
// isn't a vacuum-shaped request (no "vacuum_start" in readings — this
// topic could carry other request types too), computes IQR status,
// publishes the full computed row to EMQX for the general insert engine to
// write, and replies locally to gopub-edge with the within-IQR booleans
// for PLC write-back. Errors are logged, not fatal — a bad single message
// shouldn't kill the whole engine.
func handleRequest(ctx context.Context, localClient, emqxClient paho.Client, cfg config.Config, raw []byte) {
	var req incomingRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		log.Printf("[vacuum-engine] ⚠ failed to parse request: %v", err)
		return
	}

	if len(req.Readings) == 0 {
		return // not a vacuum request (or any readings at all) — ignore
	}
	var readings vacuumReadings
	if err := json.Unmarshal(req.Readings, &readings); err != nil {
		return // readings present but doesn't match our shape — not ours
	}
	if readings.VacuumStart == nil || readings.VacuumLeave1min == nil ||
		readings.VacuumLeave2min == nil || readings.VacuumLeave3min == nil {
		return // missing a required field — not a vacuum request
	}

	if req.ReplyTopic == "" {
		log.Printf("[vacuum-engine] ⚠ vacuum request with no reply_topic, dropping (device_id=%s)", req.DeviceID)
		return
	}
	if req.TenantID == "" || req.DeviceID == "" {
		errMsg := "missing tenant_id or device_id"
		log.Printf("[vacuum-engine] ⚠ %s", errMsg)
		_ = mqtt.PublishReply(localClient, req.ReplyTopic, false, errMsg, nil)
		return
	}

	vacuumStart := int(*readings.VacuumStart)
	v1, v2, v3 := *readings.VacuumLeave1min, *readings.VacuumLeave2min, *readings.VacuumLeave3min

	x, y := logic.CalcXY(v1, v2, v3)

	histX, histY, err := db.FetchHistoricalXY(ctx, cfg.DB, req.DeviceID, cfg.IQRHistoryLimit)
	if err != nil {
		errMsg := "failed to fetch historical data: " + err.Error()
		log.Printf("[vacuum-engine] ⚠ %s", errMsg)
		_ = mqtt.PublishReply(localClient, req.ReplyTopic, false, errMsg, nil)
		return
	}

	xStatus, yStatus, xWithin, yWithin, vacuumStatus := logic.ComputeVacuumStatus(vacuumStart, v1, v2, v3, x, y, histX, histY)

	// --- Publish the full computed row to EMQX so the general insert ---
	// --- engine records it — same shape InsertVacuumMetric used to    ---
	// --- write directly, just over MQTT instead of REST now.         ---
	metricPayload := map[string]any{
		"tenant_id":  req.TenantID,
		"device_id":  req.DeviceID,
		"resolution": "event",
		"kind":       "event",
		"readings": map[string]any{
			"vacuum_start":      vacuumStart,
			"vacuum_leave_1min": v1,
			"vacuum_leave_2min": v2,
			"vacuum_leave_3min": v3,
			"x":                 x,
			"y":                 y,
		},
		"status": map[string]any{
			"x_status":      xStatus,
			"y_status":      yStatus,
			"vacuum_status": vacuumStatus,
		},
	}
	if err := mqtt.PublishMetric(emqxClient, cfg.EMQXPublish.RequestTopic, metricPayload); err != nil {
		// Log and continue — the local reply (below) is what drives PLC
		// write-back and matters more in the moment; a dropped EMQX
		// publish means this one reading is missing from history, not a
		// broken control loop.
		log.Printf("[vacuum-engine] ⚠ failed to publish metric to EMQX: %v", err)
	}

	// --- Reply locally to gopub-edge for PLC write-back ---
	result := localReplyData{
		VacuumStart:     vacuumStart,
		VacuumLeave1min: v1,
		VacuumLeave2min: v2,
		VacuumLeave3min: v3,
		XStatus:         xStatus,
		YStatus:         yStatus,
		VacuumStatus:    vacuumStatus,
		X:               x,
		Y:               y,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}

	if err := mqtt.PublishReply(localClient, req.ReplyTopic, true, "", result); err != nil {
		log.Printf("[vacuum-engine] ⚠ failed to publish reply: %v", err)
		return
	}
	log.Printf("[vacuum-engine] ✓ processed device_id=%s x_status=%s(%v) y_status=%s(%v) vacuum_status=%v",
		req.DeviceID, xStatus, xWithin, yStatus, yWithin, vacuumStatus)
}
