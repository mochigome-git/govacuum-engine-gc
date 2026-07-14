// Package config centralizes environment loading and configuration parsing
// for vacuum-engine-gc. It loads .env.local (falling back to .env) once,
// then exposes a single Config struct built from os.Getenv so main.go
// doesn't have to know about env var names or parsing details.
package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"

	"govacuum-engine-gc/internal/db"
	"govacuum-engine-gc/internal/mqtt"
)

// Config holds everything main.go needs to wire up the engine.
type Config struct {
	DB              db.Config
	EMQX            mqtt.EMQXConfig
	Mosquitto       mqtt.MosquittoConfig
	IQRHistoryLimit int
}

// Load reads .env.local (falling back to .env) if present — best-effort,
// real env vars still win if already set — then builds a Config from the
// environment. It calls log.Fatalf for any required variable that's missing,
// matching main.go's previous behavior.
func Load() Config {
	loadDotEnv()

	historyLimit, _ := strconv.Atoi(getEnv("IQR_HISTORY_LIMIT", "1000"))

	return Config{
		DB: db.Config{
			URL:            requireEnv("SUPABASE_URL"),
			ServiceRoleKey: requireEnv("SUPABASE_SERVICE_ROLE_KEY"),
			Schema:         getEnv("SUPABASE_SCHEMA", "analytics"),
		},
		EMQX: mqtt.EMQXConfig{
			Broker:         requireEnv("EMQX_HOST"),
			Port:           getEnv("EMQX_PORT", "8883"),
			Username:       os.Getenv("EMQX_USERNAME"),
			Password:       os.Getenv("EMQX_PASSWORD"),
			UseTLS:         mustParseBool(getEnv("EMQX_TLS_ON", "true")),
			CACert:         os.Getenv("EMQX_CA_CERTIFICATE"),
			ClientIDPrefix: getEnv("EMQX_CLIENT_ID_PREFIX", "vacuum-engine_"),
			RequestTopic:   requireEnv("MQTT_INSERT_REQUEST_TOPIC"), // must match gopub-edge's setting exactly
		},
		Mosquitto: mqtt.MosquittoConfig{
			Broker:         requireEnv("MOSQUITTO_HOST"),
			Port:           getEnv("MOSQUITTO_PORT", "1883"),
			UseTLS:         mustParseBool(getEnv("MOSQUITTO_TLS_ON", "false")),
			CACert:         os.Getenv("MOSQUITTO_CA_CERTIFICATE"),
			ClientIDPrefix: getEnv("MOSQUITTO_CLIENT_ID_PREFIX", "vacuum-engine-reply_"),
		},
		IQRHistoryLimit: historyLimit,
	}
}

// loadDotEnv tries .env.local first, and only falls back to .env if
// .env.local itself doesn't exist. godotenv.Load(".env.local", ".env")
// looks like a fallback but isn't — it loads both files in order and
// bails out on the first missing one, so if .env.local is absent it
// never even tries .env. This does the fallback the caller actually wants.
// Both files are optional — real env vars set in the shell always win over
// either.
func loadDotEnv() {
	if _, err := os.Stat(".env.local"); err == nil {
		if err := godotenv.Load(".env.local"); err != nil {
			log.Printf("[config] found .env.local but failed to load it: %v", err)
		}
		return
	}
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(".env"); err != nil {
			log.Printf("[config] found .env but failed to load it: %v", err)
		}
	}
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
