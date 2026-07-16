package audio

import "testing"

func TestPCMFrameQueueTransfersSlotOwnershipWithoutCopying(t *testing.T) {
	queue := newPCMFrameQueue(2)
	write, ok := queue.AcquireWrite()
	if !ok {
		t.Fatal("AcquireWrite() failed")
	}
	write.Samples[0] = 0.25
	write.Timestamp = 480
	queue.CommitWrite()
	read, ok := queue.AcquireRead()
	if !ok {
		t.Fatal("AcquireRead() failed")
	}
	if read != write || read.Samples[0] != 0.25 || read.Timestamp != 480 {
		t.Fatalf("read slot = %#v", read)
	}
	queue.ReleaseRead()
}

func TestPCMFrameQueueDoesNotReuseHeldSlot(t *testing.T) {
	queue := newPCMFrameQueue(1)
	write, _ := queue.AcquireWrite()
	queue.CommitWrite()
	read, _ := queue.AcquireRead()
	if read != write {
		t.Fatal("consumer did not acquire the committed slot")
	}
	if _, ok := queue.AcquireWrite(); ok {
		t.Fatal("producer reused a consumer-held slot")
	}
	queue.ReleaseRead()
	if _, ok := queue.AcquireWrite(); !ok {
		t.Fatal("producer could not reuse a released slot")
	}
}
