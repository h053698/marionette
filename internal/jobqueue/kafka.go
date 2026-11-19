// Package jobqueue carries only opaque job IDs; credentials and action payloads
// remain in the backend's private durable store.
package jobqueue

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
)

type Config struct {
	Brokers            []string
	Topic              string
	TLS                bool
	Username, Password string
}

func FromEnv() Config {
	c := Config{Topic: os.Getenv("MARIONETTE_KAFKA_TOPIC"), TLS: os.Getenv("MARIONETTE_KAFKA_TLS") == "true", Username: os.Getenv("MARIONETTE_KAFKA_USERNAME"), Password: os.Getenv("MARIONETTE_KAFKA_PASSWORD")}
	for _, broker := range strings.Split(os.Getenv("MARIONETTE_KAFKA_BROKERS"), ",") {
		if broker = strings.TrimSpace(broker); broker != "" {
			c.Brokers = append(c.Brokers, broker)
		}
	}
	if c.Topic == "" {
		c.Topic = "marionette.jobs"
	}
	return c
}

func New(c Config, group string) (*kgo.Client, error) {
	if len(c.Brokers) == 0 {
		return nil, errors.New("MARIONETTE_KAFKA_BROKERS is required")
	}
	if c.Username != "" && (!c.TLS || c.Password == "") {
		return nil, errors.New("Kafka SASL/PLAIN requires TLS and a password")
	}
	options := []kgo.Opt{kgo.SeedBrokers(c.Brokers...), kgo.DefaultProduceTopic(c.Topic), kgo.RequiredAcks(kgo.AllISRAcks()), kgo.ProducerBatchCompression(kgo.NoCompression()), kgo.DialTimeout(5 * time.Second), kgo.ProduceRequestTimeout(5 * time.Second), kgo.RecordDeliveryTimeout(10 * time.Second)}
	if c.TLS {
		options = append(options, kgo.Dialer((&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12}}).DialContext))
	}
	if c.Username != "" {
		options = append(options, kgo.SASL(plain.Auth{User: c.Username, Pass: c.Password}.AsMechanism()))
	}
	if group != "" {
		options = append(options, kgo.ConsumerGroup(group), kgo.ConsumeTopics(c.Topic), kgo.DisableAutoCommit(), kgo.BlockRebalanceOnPoll(), kgo.RebalanceTimeout(60*time.Minute))
	}
	return kgo.NewClient(options...)
}

func Publish(ctx context.Context, producer *kgo.Client, topic, id string) error {
	return producer.ProduceSync(ctx, &kgo.Record{Topic: topic, Key: []byte(id), Value: []byte(id)}).FirstErr()
}
