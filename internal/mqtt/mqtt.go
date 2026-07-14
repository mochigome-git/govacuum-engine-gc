// Package mqtt wraps paho for govacuum-engine-gc's two connections:
// EMQX (subscribe to the shared insert-request topic) and the local edge
// Mosquitto (publish replies). Exported so package main can call it.
package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

// EMQXConfig mirrors gopub-edge's publisher-side connection settings —
// same broker, since this engine consumes what gopub-edge publishes there.
type EMQXConfig struct {
	Broker         string
	Port           string
	Username       string
	Password       string
	UseTLS         bool
	CACert         string
	ClientIDPrefix string
	RequestTopic   string // e.g. "gim/devices/payload" — must match gopub-edge's MQTT_INSERT_REQUEST_TOPIC exactly
}

// MosquittoConfig is the local edge Mosquitto broker — where replies
// actually get published. Typically unauthenticated on a LAN, so
// Username/Password are intentionally absent; add them if your instance
// requires auth.
type MosquittoConfig struct {
	Broker         string
	Port           string
	UseTLS         bool
	CACert         string
	ClientIDPrefix string
}

// ConnectRequestSubscriber connects to EMQX and subscribes to the shared
// insert-request topic. onMessage fires for every message on that topic,
// including ones this engine doesn't care about (it's shared with other
// consumers) — filtering happens in the caller's handler.
func ConnectRequestSubscriber(cfg EMQXConfig, onMessage paho.MessageHandler) (paho.Client, error) {
	opts := paho.NewClientOptions()

	// "ssl", not "mqtts" — paho's scheme parser reliably recognizes
	// tcp/ssl/tls/ws/wss but not "mqtts".
	scheme := "tcp"
	if cfg.UseTLS {
		scheme = "ssl"
	}
	opts.AddBroker(fmt.Sprintf("%s://%s:%s", scheme, cfg.Broker, cfg.Port))

	prefix := cfg.ClientIDPrefix
	if prefix == "" {
		prefix = "vacuum-engine_"
	}
	opts.SetClientID(prefix + uuid.New().String())
	opts.SetUsername(cfg.Username)
	opts.SetPassword(cfg.Password)
	opts.SetAutoReconnect(true)

	if cfg.UseTLS {
		tlsConfig, err := buildTLSConfig(cfg.CACert)
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tlsConfig)
	}

	client := paho.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return nil, fmt.Errorf("EMQX connect: %w", token.Error())
	}

	if token := client.Subscribe(cfg.RequestTopic, 1, onMessage); token.Wait() && token.Error() != nil {
		client.Disconnect(250)
		return nil, fmt.Errorf("EMQX subscribe to %s: %w", cfg.RequestTopic, token.Error())
	}
	log.Printf("[vacuum-engine] ✓ connected to EMQX — listening on %q", cfg.RequestTopic)

	return client, nil
}

// ConnectReplyPublisher connects to the local edge Mosquitto broker —
// publish-only, no subscription needed here.
func ConnectReplyPublisher(cfg MosquittoConfig) (paho.Client, error) {
	opts := paho.NewClientOptions()

	scheme := "tcp"
	if cfg.UseTLS {
		scheme = "ssl"
	}
	opts.AddBroker(fmt.Sprintf("%s://%s:%s", scheme, cfg.Broker, cfg.Port))

	prefix := cfg.ClientIDPrefix
	if prefix == "" {
		prefix = "vacuum-engine-reply_"
	}
	opts.SetClientID(prefix + uuid.New().String())
	opts.SetAutoReconnect(true)

	if cfg.UseTLS {
		tlsConfig, err := buildTLSConfig(cfg.CACert)
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tlsConfig)
	}

	client := paho.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return nil, fmt.Errorf("Mosquitto connect: %w", token.Error())
	}
	log.Printf("[vacuum-engine] ✓ connected to local Mosquitto for replies")

	return client, nil
}

func buildTLSConfig(caCert string) (*tls.Config, error) {
	tlsConfig := &tls.Config{}
	if caCert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caCert)) {
			return nil, fmt.Errorf("failed to append CA certificate")
		}
		tlsConfig.RootCAs = pool
	}
	return tlsConfig, nil
}

// replyPayload mirrors gopub-edge's mqttpub.ReplyPayload shape exactly —
// {success, error, data} — so SendUpsertRequest's parsing works unchanged.
type replyPayload struct {
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// PublishReply publishes {success, error, data} to replyTopic.
func PublishReply(client paho.Client, replyTopic string, success bool, errMsg string, data any) error {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshal reply data: %w", err)
		}
		raw = b
	}

	payload, err := json.Marshal(replyPayload{Success: success, Error: errMsg, Data: raw})
	if err != nil {
		return fmt.Errorf("marshal reply envelope: %w", err)
	}

	token := client.Publish(replyTopic, 1, false, payload)
	token.Wait()
	return token.Error()
}
