package kafkaconsumer

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Shopify/sarama"
	"github.com/wvanbergen/kazoo-go"
	"gopkg.in/tomb.v1"
)

// partitionManager manages the consumption of a single partition, and committing
// the processed messages to offset storage.
type partitionManager struct {
	parent    *consumerManager
	t         tomb.Tomb
	partition *kazoo.Partition

	offsetManager      sarama.PartitionOffsetManager
	lastConsumedOffset int64
	processingDone     chan struct{}
}

// run implements the main partition manager loop.
// 1. Claim the partition in Zookeeper
// 2. Determine at what offset to start consuming
// 3. Start a consumer for the partition at the inital offset
// 4. Transfer messages and errors from the partition consumer to the consumer manager.
func (pm *partitionManager) run() {
	defer pm.t.Done()

	if err := pm.claimPartition(); err != nil {
		pm.t.Kill(err)
		return
	}
	defer pm.releasePartition()

	offsetManager, err := pm.startPartitionOffsetManager()
	if err != nil {
		pm.t.Kill(err)
		return
	} else {
		pm.offsetManager = offsetManager
		defer offsetManager.Close()
	}

	// We are ignoring metadata for now.
	initialOffset, _ := offsetManager.Offset()
	if initialOffset < 0 {
		initialOffset = pm.parent.config.Offsets.Initial
	} else {
		// Fix the off by one error: we should start consuming once message after the last committed offset
		initialOffset += 1
	}
	defer pm.waitForProcessing()

	pc, err := pm.startPartitionConsumer(initialOffset)
	if err != nil {
		pm.t.Kill(err)
		return
	}
	defer pm.closePartitionConsumer(pc)

	for {
		select {
		case <-pm.t.Dying():
			return

		case msg := <-pc.Messages():
			select {
			case pm.parent.messages <- msg:
				atomic.StoreInt64(&pm.lastConsumedOffset, msg.Offset)

			case <-pm.t.Dying():
				return
			}

		case err := <-offsetManager.Errors():
			select {
			case pm.parent.errors <- err:
				// Noop?
			case <-pm.t.Dying():
				return
			}

		case err := <-pc.Errors():
			select {
			case pm.parent.errors <- err:
				// Noop?
			case <-pm.t.Dying():
				return
			}
		}
	}
}

// startPartitionOffsetManager starts a PartitionOffsetManager for the partition, and will
// retry any errors. The only error value that can be returned is tomb.ErrDying, which is
// returned when the partition manager is interrupted. Any other error should be considered
// non-recoverable.
func (pm *partitionManager) startPartitionOffsetManager() (sarama.PartitionOffsetManager, error) {
	for {
		offsetManager, err := pm.parent.offsetManager.ManagePartition(pm.partition.Topic().Name, pm.partition.ID)
		if err != nil {
			Logger.Printf("Failed to start partition offset manager for %s: %s. Trying again in 1 second...\n", pm.partition.Key(), err)

			select {
			case <-pm.t.Dying():
				return nil, tomb.ErrDying
			case <-time.After(1 * time.Second):
				continue
			}
		}

		return offsetManager, nil
	}
}

// waitForProcessing waits for all the messages that were consumed for this partition to be processed.
// The processing can take at most MaxProcessingTime time. After that, those messages are consisered
// lost and will not be committed. Note that this may cause messages to be processed twice if another
// partition consumer resumes consuming from this partition later.
func (pm *partitionManager) waitForProcessing() {
	lastProcessedOffset, _ := pm.offsetManager.Offset()
	lastConsumedOffset := atomic.LoadInt64(&pm.lastConsumedOffset)

	if lastConsumedOffset >= 0 {
		if lastConsumedOffset > lastProcessedOffset {
			Logger.Printf("Waiting for offset %d to be processed before stopping %s...", lastConsumedOffset, pm.partition.Key())

			select {
			case <-pm.processingDone:
				Logger.Printf("Offset %d has been processed for %s, continuing shutdown.", lastConsumedOffset, pm.partition.Key())
			case <-time.After(pm.parent.config.MaxProcessingTime):

				Logger.Printf("TIMEOUT: offset %d still has not been processed for %s. The last processed offset was %d.", lastConsumedOffset, pm.partition.Key(), lastProcessedOffset)
			}
		} else {
			Logger.Printf("Offset %d has been processed for %s. Continuing shutdown...", lastConsumedOffset, pm.partition.Key())
		}
	}
}

// interrupt initiates the shutdown procedure for the partition manager, and returns immediately.
func (pm *partitionManager) interrupt() {
	pm.t.Kill(nil)
}

// close starts the shutdown proecure, and waits for it to complete.
func (pm *partitionManager) close() error {
	pm.interrupt()
	return pm.t.Wait()
}

// ack sets the offset on the partition's offset manager, and signals that
// processing done if the offset is equal to the last consumed offset during shutdown.
func (pm *partitionManager) ack(offset int64) {
	pm.offsetManager.SetOffset(offset, "")

	if pm.t.Err() != tomb.ErrStillAlive && offset == atomic.LoadInt64(&pm.lastConsumedOffset) {
		close(pm.processingDone)
	}
}

// claimPartition claims a partition in Zookeeper for this instance.
// If the partition is already claimed by someone else, it will wait for the
// partition to become available. It will retry errors if they occur.
// This method should therefore only return with a nil error value, or
// tomb.ErrDying if the partitionManager was interrupted. Any other errors
// are not recoverable.
func (pm *partitionManager) claimPartition() error {
	Logger.Printf("Trying to claim partition %s...", pm.partition.Key())

	for {
		owner, changed, err := pm.parent.group.WatchPartitionOwner(pm.partition.Topic().Name, pm.partition.ID)
		if err != nil {
			Logger.Printf("Failed to get partition owner for %s from Zookeeper: %s. Trying again in 1 second...", pm.partition.Key(), err)
			select {
			case <-time.After(1 * time.Second):
				continue
			case <-pm.t.Dying():
				return tomb.ErrDying
			}
		}

		if owner != nil {
			if owner.ID == pm.parent.instance.ID {
				return fmt.Errorf("The current instance is already the owner of %s. This should not happen.", pm.partition.Key())
			}

			Logger.Printf("Partition %s is currently claimed by instance %s. Waiting for it to be released...", pm.partition.Key(), owner.ID)
			select {
			case <-changed:
				continue
			case <-pm.t.Dying():
				return tomb.ErrDying
			}

		} else {

			err := pm.parent.instance.ClaimPartition(pm.partition.Topic().Name, pm.partition.ID)
			if err != nil {
				Logger.Printf("Fail to claim ownership for %s: %s. Trying again...", pm.partition.Key(), err)
				continue
			}

			Logger.Printf("Claimed ownership for %s", pm.partition.Key())
			return nil
		}
	}
}

// closePartitionConsumer starts a sarama consumer for the partition under management.
// This function will retry any error that may occur. The error return value is nil once
// it successfully has started the partition consumer, or tomb.ErrDying if the partition
// manager was interrupted. Any other error is not recoverable.
func (pm *partitionManager) startPartitionConsumer(initialOffset int64) (sarama.PartitionConsumer, error) {
	var (
		pc  sarama.PartitionConsumer
		err error
	)

	for {
		pc, err = pm.parent.consumer.ConsumePartition(pm.partition.Topic().Name, pm.partition.ID, initialOffset)
		switch err {
		case nil:
			switch initialOffset {
			case sarama.OffsetNewest:
				Logger.Printf("Started consumer for %s for new messages only.", pm.partition.Key())
			case sarama.OffsetOldest:
				Logger.Printf("Started consumer for %s at the oldest available offset.", pm.partition.Key())
			default:
				Logger.Printf("Started consumer for %s at offset %d.", pm.partition.Key(), initialOffset)
			}

			// We have a valid partition consumer so we can return
			return pc, nil

		case sarama.ErrOffsetOutOfRange:
			// The offset we had on file is too old. Restart with initial offset
			if pm.parent.config.Offsets.Initial == sarama.OffsetNewest {
				Logger.Printf("Offset %d is no longer available for %s. Trying again with new messages only...", initialOffset, pm.partition.Key())
			} else if pm.parent.config.Offsets.Initial == sarama.OffsetOldest {
				Logger.Printf("Offset %d is no longer available for %s. Trying again with he oldest available offset...", initialOffset, pm.partition.Key())
			}
			initialOffset = pm.parent.config.Offsets.Initial

			continue

		default:
			// Assume the problem is temporary; just try again.
			// FIXME: Do we want to treat all errors like this?
			// FIXME: Should te sleep by configurable?
			Logger.Printf("Failed to start consuming partition for %s: %s. Trying again in 1 second...\n", pm.partition.Key(), err)
			select {
			case <-pm.t.Dying():
				return nil, tomb.ErrDying
			case <-time.After(1 * time.Second):
				continue
			}
		}

	}
}

// closePartitionConsumer closes the sarama consumer for the partition under management.
func (pm *partitionManager) closePartitionConsumer(pc sarama.PartitionConsumer) {
	if err := pc.Close(); err != nil {
		Logger.Printf("Failed to close partition consumer for %s: %s\n", pm.partition.Key(), err)
	}
}

// releasePartition releases this instance's claim on this partition in Zookeeper.
func (pm *partitionManager) releasePartition() {
	if err := pm.parent.instance.ReleasePartition(pm.partition.Topic().Name, pm.partition.ID); err != nil {
		Logger.Printf("FAILED to release partition %s: %s", pm.partition.Key(), err)
	} else {
		Logger.Printf("Released partition %s.", pm.partition.Key())
	}
}
