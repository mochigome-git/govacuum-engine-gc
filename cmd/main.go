package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"

	"govacuum-engine-gc/internal/db"
	"govacuum-engine-gc/internal/logic"
	"govacuum-engine-gc/internal/mqtt"
)

// incomingRequest is the flat envelope gopub-edge publishes — see its
// mqttpub package. Only the fields this engine cares about are declared;
// anything else in the JSON is ignored by json.Unmarshal.
type incomingRequest struct {
	TenantID   string          `json:"tenant_id"`
	DeviceID   string          `json:"device_id"`
	ReplyTopic string          `json:"reply_topic"`
	Readings   json.RawMessage `json:"readings"`
}

// vacuumReadings is what we expect inside "readings" for a vacuum request.
// vacuum_start/vacuum_leave_* come in as gopub-edge sends them — declared
// loosely as float64 since PLC values arrive as float64 from the MQTT JSON,
// matching patch.VacuumData's existing (slightly odd but established)
// convention of float64 even where the DB column is integer.
type vacuumReadings struct {
	VacuumStart     *float64 `json:"vacuum_start"`
	VacuumLeave1min *float64 `json:"vacuum_leave_1min"`
	VacuumLeave2min *float64 `json:"vacuum_leave_2min"`
	VacuumLeave3min *float64 `json:"vacuum_leave_3min"`
}

// vacuumData mirrors gopub-edge's patch.VacuumData exactly — this is what
// gets marshaled into the reply's "data" field, so SendUpsertRequest's
// existing parsing (and PLC write-back) works completely unchanged.
type vacuumData struct {
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
	_ = godotenv.Load(".env.local", ".env") // best-effort, falls back to real env

	dbCfg := db.Config{
		URL:            requireEnv("SUPABASE_URL"),
		ServiceRoleKey: requireEnv("SUPABASE_SERVICE_ROLE_KEY"),
		Schema:         getEnv("SUPABASE_SCHEMA", "analytics"),
	}

	historyLimit, _ := strconv.Atoi(getEnv("IQR_HISTORY_LIMIT", "1000"))

	emqxCfg := mqtt.EMQXConfig{
		Broker:         requireEnv("EMQX_HOST"),
		Port:           getEnv("EMQX_PORT", "8883"),
		Username:       os.Getenv("EMQX_USERNAME"),
		Password:       os.Getenv("EMQX_PASSWORD"),
		UseTLS:         mustParseBool(getEnv("EMQX_TLS_ON", "true")),
		CACert:         os.Getenv("EMQX_CA_CERTIFICATE"),
		ClientIDPrefix: getEnv("EMQX_CLIENT_ID_PREFIX", "vacuum-engine_"),
		RequestTopic:   requireEnv("MQTT_INSERT_REQUEST_TOPIC"), // must match gopub-edge's setting exactly
	}

	mosquittoCfg := mqtt.MosquittoConfig{
		Broker:         requireEnv("MOSQUITTO_HOST"),
		Port:           getEnv("MOSQUITTO_PORT", "1883"),
		UseTLS:         mustParseBool(getEnv("MOSQUITTO_TLS_ON", "false")),
		CACert:         os.Getenv("MOSQUITTO_CA_CERTIFICATE"),
		ClientIDPrefix: getEnv("MOSQUITTO_CLIENT_ID_PREFIX", "vacuum-engine-reply_"),
	}

	replyClient, err := mqtt.ConnectReplyPublisher(mosquittoCfg)
	if err != nil {
		log.Fatalf("failed to connect to Mosquitto: %v", err)
	}
	defer replyClient.Disconnect(250)

	handler := func(_ paho.Client, msg paho.Message) {
		handleRequest(context.Background(), replyClient, dbCfg, historyLimit, msg.Payload())
	}

	requestClient, err := mqtt.ConnectRequestSubscriber(emqxCfg, handler)
	if err != nil {
		log.Fatalf("failed to connect MQTT: %v", err)
	}
	defer requestClient.Disconnect(250)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("vacuum-engine shutting down")
}

// handleRequest parses one incoming message, ignores anything that isn't a
// vacuum-shaped request (no "vacuum_start" in readings — this topic is
// shared with other request types), computes IQR status, inserts, and
// replies. Errors are logged, not fatal — a bad single message shouldn't
// kill the whole engine.
func handleRequest(ctx context.Context, client paho.Client, dbCfg db.Config, historyLimit int, raw []byte) {
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
		_ = mqtt.PublishReply(client, req.ReplyTopic, false, errMsg, nil)
		return
	}

	vacuumStart := int(*readings.VacuumStart)
	v1, v2, v3 := *readings.VacuumLeave1min, *readings.VacuumLeave2min, *readings.VacuumLeave3min

	x, y := logic.CalcXY(v1, v2, v3)

	histX, histY, err := db.FetchHistoricalXY(ctx, dbCfg, req.DeviceID, historyLimit)
	if err != nil {
		errMsg := "failed to fetch historical data: " + err.Error()
		log.Printf("[vacuum-engine] ⚠ %s", errMsg)
		_ = mqtt.PublishReply(client, req.ReplyTopic, false, errMsg, nil)
		return
	}

	xStatus, yStatus, vacuumStatus := logic.ComputeVacuumStatus(vacuumStart, v1, v2, v3, x, y, histX, histY)

	id, createdAt, err := db.InsertVacuumMetric(ctx, dbCfg, req.TenantID, req.DeviceID,
		vacuumStart, v1, v2, v3, x, y, xStatus, yStatus, vacuumStatus)
	if err != nil {
		errMsg := "failed to insert metric: " + err.Error()
		log.Printf("[vacuum-engine] ⚠ %s", errMsg)
		_ = mqtt.PublishReply(client, req.ReplyTopic, false, errMsg, nil)
		return
	}

	result := vacuumData{
		ID:              id,
		CreatedAt:       createdAt.Format(time.RFC3339),
		VacuumStart:     vacuumStart,
		VacuumLeave1min: v1,
		VacuumLeave2min: v2,
		VacuumLeave3min: v3,
		XStatus:         xStatus,
		YStatus:         yStatus,
		VacuumStatus:    vacuumStatus,
		X:               x,
		Y:               y,
	}

	if err := mqtt.PublishReply(client, req.ReplyTopic, true, "", result); err != nil {
		log.Printf("[vacuum-engine] ⚠ failed to publish reply: %v", err)
		return
	}
	log.Printf("[vacuum-engine] ✓ processed device_id=%s x_status=%s y_status=%s vacuum_status=%v",
		req.DeviceID, xStatus, yStatus, vacuumStatus)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is not set", key)
	}
	return v
}

func mustParseBool(s string) bool {
	b, _ := strconv.ParseBool(s)
	return b
}
