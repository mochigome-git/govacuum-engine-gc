// Package mqtt wraps paho for govacuum-engine-gc's two connections:
//
//   - Local (edge Mosquitto): gopub-edge and vacuum-engine run on the same
//     Pi, so the upsert REQUEST now arrives here directly instead of
//     round-tripping through EMQX — ConnectLocal both subscribes to the
//     request topic AND is reused afterward to publish the reply, one
//     connection doing both jobs (same pattern gopub-edge itself uses for
//     its own Mosquitto connection).
//
//   - EMQX: used only to publish the finished, fully-computed metric
//     (original readings + derived x/y + status) to
//     MQTT_INSERT_REQUEST_TOPIC — the same topic ordinary readings rows
//     already go through, so the existing general insert engine writes it
//     with no special-casing. No subscription happens on this client
//     anymore.
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

// LocalConfig is the local edge Mosquitto broker. RequestTopic must match
// gopub-edge's LOCAL_VACUUM_REQUEST_TOPIC exactly — that's the topic
// gopub-edge now publishes vacuum upsert requests to directly.
type LocalConfig struct {
	Broker         string
	Port           string
	UseTLS         bool
	CACert         string
	ClientIDPrefix string
	RequestTopic   string
}

// EMQXPublishConfig is EMQX, used only to publish the finished computed
// metric. RequestTopic here is MQTT_INSERT_REQUEST_TOPIC — same topic
// gopub-edge's plain "patch" readings already publish to.
type EMQXPublishConfig struct {
	Broker         string
	Port           string
	Username       string
	Password       string
	UseTLS         bool
	CACert         string
	ClientIDPrefix string
	RequestTopic   string
}

// ConnectLocal connects to the local Mosquitto broker and subscribes to
// cfg.RequestTopic. The returned client is also what you publish replies
// on (see PublishReply) — no separate connection needed since it's all
// one local broker.
func ConnectLocal(cfg LocalConfig, onMessage paho.MessageHandler) (paho.Client, error) {
	opts := paho.NewClientOptions()

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

	if token := client.Subscribe(cfg.RequestTopic, 1, onMessage); token.Wait() && token.Error() != nil {
		client.Disconnect(250)
		return nil, fmt.Errorf("Mosquitto subscribe to %s: %w", cfg.RequestTopic, token.Error())
	}
	log.Printf("[vacuum-engine] ✓ connected to local Mosquitto — listening on %q, replies go out on this same connection", cfg.RequestTopic)

	return client, nil
}

// ConnectEMQXPublisher connects to EMQX for one purpose only: publishing
// the finished computed metric to cfg.RequestTopic. No subscription.
func ConnectEMQXPublisher(cfg EMQXPublishConfig) (paho.Client, error) {
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
		prefix = "vacuum-engine-publisher_"
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
	log.Printf("[vacuum-engine] ✓ connected to EMQX — publishing finished metrics to %q", cfg.RequestTopic)

	return client, nil
}

// PublishMetric marshals payload and publishes it to topic (the EMQX
// client, MQTT_INSERT_REQUEST_TOPIC) — shaped as a normal readings-style
// envelope so the general insert engine handles it like any other device
// payload, no special-casing needed on the receiving end.
func PublishMetric(client paho.Client, topic string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal metric payload: %w", err)
	}
	token := client.Publish(topic, 1, false, b)
	token.Wait()
	return token.Error()
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

// PublishReply publishes {success, error, data} to replyTopic, over the
// same local Mosquitto client ConnectLocal returned.
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
