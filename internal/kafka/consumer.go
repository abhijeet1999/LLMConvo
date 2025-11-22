package kafka

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/IBM/sarama"
)

type Consumer struct {
	consumer sarama.Consumer
	handler  func(message []byte) error
}

func NewConsumer(brokers []string, handler func(message []byte) error) (*Consumer, error) {
	config := sarama.NewConfig()
	config.Consumer.Return.Errors = true

	consumer, err := sarama.NewConsumer(brokers, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}

	return &Consumer{
		consumer: consumer,
		handler:  handler,
	}, nil
}

func (c *Consumer) Consume(topic string) error {
	for {
		partitionConsumer, err := c.consumer.ConsumePartition(topic, 0, sarama.OffsetNewest)
		if err != nil {
			// Topic might not exist yet, wait and retry
			log.Printf("Failed to consume partition %s: %v, retrying in 2s...", topic, err)
			time.Sleep(2 * time.Second)
			continue
		}

		log.Printf("Consuming messages from topic: %s", topic)

		reconnect := false
		for !reconnect {
			select {
			case message := <-partitionConsumer.Messages():
				if message == nil {
					// Channel closed, topic might have been deleted
					partitionConsumer.Close()
					log.Printf("Topic %s channel closed, reconnecting...", topic)
					time.Sleep(1 * time.Second)
					reconnect = true
					break
				}
				if err := c.handler(message.Value); err != nil {
					log.Printf("Error handling message: %v", err)
				}
			case err := <-partitionConsumer.Errors():
				if err != nil {
					log.Printf("Consumer error for topic %s: %v", topic, err.Err)
					// If topic was deleted, close and reconnect
					errStr := err.Err.Error()
					if strings.Contains(errStr, "outside the range") ||
						strings.Contains(errStr, "not found") ||
						strings.Contains(errStr, "does not exist") {
						partitionConsumer.Close()
						log.Printf("Topic %s was deleted or not found, reconnecting...", topic)
						time.Sleep(1 * time.Second)
						reconnect = true
						break
					}
				}
			}
		}
	}
}

func (c *Consumer) Close() error {
	return c.consumer.Close()
}

func UnmarshalMessage(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
