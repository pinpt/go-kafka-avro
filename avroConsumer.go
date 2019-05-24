package kafka

import (
	"encoding/binary"
	"os"
	"os/signal"
	"time"

	"github.com/Shopify/sarama"
	"github.com/bsm/sarama-cluster"
	"github.com/linkedin/goavro"
)

type avroConsumer struct {
	Consumer             *cluster.Consumer
	SchemaRegistryClient *CachedSchemaRegistryClient
	callbacks            ConsumerCallbacks
	config               *cluster.Config
}

type ConsumerCallbacks struct {
	OnDataReceived func(msg Message)
	OnError        func(err error)
	OnNotification func(notification *cluster.Notification)
}

type Message struct {
	SchemaId  int
	Topic     string
	Partition int32
	Offset    int64
	Key       string
	Value     string

	Headers   map[string]string
	Timestamp time.Time // only set if kafka is version 0.10+, inner message timestamp
}

func NewDefaultConfig() *cluster.Config {
	config := cluster.NewConfig()
	config.Consumer.Return.Errors = true
	config.Group.Return.Notifications = true
	//read from beginning at the first time
	config.Consumer.Offsets.Initial = sarama.OffsetOldest
	return config
}

// NewAvroConsumerWithConfig returns a basic consumer to interact with schema registry, avro and kafka and uses the passed in config
func NewAvroConsumerWithConfig(kafkaServers []string, schemaRegistryServers []string,
	topic string, groupId string, callbacks ConsumerCallbacks, config *cluster.Config) (*avroConsumer, error) {
	// init (custom) config, enable errors and notifications
	topics := []string{topic}
	consumer, err := cluster.NewConsumer(kafkaServers, groupId, topics, config)
	if err != nil {
		return nil, err
	}

	schemaRegistryClient := NewCachedSchemaRegistryClient(schemaRegistryServers)
	return &avroConsumer{
		consumer,
		schemaRegistryClient,
		callbacks,
		config,
	}, nil
}

// NewAvroConsumer returns a basic consumer to interact with schema registry, avro and kafka
func NewAvroConsumer(kafkaServers []string, schemaRegistryServers []string,
	topic string, groupId string, callbacks ConsumerCallbacks) (*avroConsumer, error) {
	// init (custom) config, enable errors and notifications
	return NewAvroConsumerWithConfig(kafkaServers, schemaRegistryServers, topic, groupId, callbacks, NewDefaultConfig())
}

//GetSchemaId get schema id from schema-registry service
func (ac *avroConsumer) GetSchema(id int) (*goavro.Codec, error) {
	codec, err := ac.SchemaRegistryClient.GetSchema(id)
	if err != nil {
		return nil, err
	}
	return codec, nil
}

func (ac *avroConsumer) Consume() {
	// trap SIGINT to trigger a shutdown.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	if ac.config.Consumer.Return.Errors {
		// consume errors
		go func() {
			for err := range ac.Consumer.Errors() {
				if ac.callbacks.OnError != nil {
					ac.callbacks.OnError(err)
				}
			}
		}()
	}

	if ac.config.Group.Return.Notifications {
		// consume notifications
		go func() {
			for notification := range ac.Consumer.Notifications() {
				if ac.callbacks.OnNotification != nil {
					ac.callbacks.OnNotification(notification)
				}
			}
		}()
	}

	for {
		select {
		case m, ok := <-ac.Consumer.Messages():
			if ok {
				msg, err := ac.ProcessAvroMsg(m)
				if err != nil {
					ac.callbacks.OnError(err)
				} else {
					if ac.callbacks.OnDataReceived != nil {
						ac.callbacks.OnDataReceived(msg)
					}
				}
				ac.Consumer.MarkOffset(m, "")
			}
		case <-signals:
			return
		}
	}
}

func (ac *avroConsumer) ProcessAvroMsg(m *sarama.ConsumerMessage) (Message, error) {
	schemaId := binary.BigEndian.Uint32(m.Value[1:5])
	codec, err := ac.GetSchema(int(schemaId))
	if err != nil {
		return Message{}, err
	}
	// Convert binary Avro data back to native Go form
	native, _, err := codec.NativeFromBinary(m.Value[5:])
	if err != nil {
		return Message{}, err
	}

	// Convert native Go form to textual Avro data
	textual, err := codec.TextualFromNative(nil, native)

	if err != nil {
		return Message{}, err
	}
	msg := Message{int(schemaId), m.Topic, m.Partition, m.Offset, string(m.Key), string(textual), nil, m.Timestamp}
	if m.Headers != nil {
		msg.Headers = make(map[string]string)
		for _, v := range m.Headers {
			msg.Headers[string(v.Key)] = string(v.Value)
		}
	}
	return msg, nil
}

func (ac *avroConsumer) Close() error {
	return ac.Consumer.Close()
}
