package kafka

import (
	"fmt"
	"log"

	"github.com/IBM/sarama"
)

// DeleteTopics deletes the specified Kafka topics
func DeleteTopics(brokers []string, topics []string) error {
	config := sarama.NewConfig()
	config.Version = sarama.V2_0_0_0

	admin, err := sarama.NewClusterAdmin(brokers, config)
	if err != nil {
		return fmt.Errorf("failed to create admin client: %w", err)
	}
	defer admin.Close()

	for _, topic := range topics {
		if err := admin.DeleteTopic(topic); err != nil {
			log.Printf("Failed to delete topic %s: %v", topic, err)
			// Continue with other topics even if one fails
		} else {
			log.Printf("Deleted topic: %s", topic)
		}
	}

	return nil
}

