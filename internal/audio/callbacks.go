package audio

import "sync/atomic"

type captureAssembler struct {
	queue      *pcmFrameQueue
	ready      chan<- struct{}
	generation *atomic.Uint64

	current     *pcmFrame
	frameOffset int
	dropFrame   bool
	silentFrame bool
	sampleClock uint32
}

func (a *captureAssembler) Write(samples []float32, muted bool) {
	for len(samples) > 0 {
		if a.frameOffset == 0 {
			a.dropFrame = false
			a.current = nil
			a.current, _ = a.queue.AcquireWrite()
			a.dropFrame = a.current == nil
			a.silentFrame = muted
			if a.current != nil && a.generation != nil {
				a.current.Generation = a.generation.Load()
			}
		}
		if muted {
			a.silentFrame = true
		}
		count := min(len(samples), FrameSamples-a.frameOffset)
		if !a.dropFrame && !a.silentFrame {
			copy(a.current.Samples[a.frameOffset:a.frameOffset+count], samples[:count])
		}
		a.frameOffset += count
		a.sampleClock += uint32(count)
		samples = samples[count:]
		if a.frameOffset == FrameSamples {
			if !a.dropFrame {
				if a.silentFrame {
					clear(a.current.Samples[:])
				}
				a.current.Timestamp = a.sampleClock
				a.queue.CommitWrite()
				select {
				case a.ready <- struct{}{}:
				default:
				}
			}
			a.current = nil
			a.frameOffset = 0
		}
	}
}

type playbackReader struct {
	queue  *pcmFrameQueue
	wake   chan<- struct{}
	demand *atomic.Uint64

	current       *pcmFrame
	currentOffset int
	currentIndex  uint64
}

func newPlaybackReader(queue *pcmFrameQueue, wake chan<- struct{}, demand *atomic.Uint64) *playbackReader {
	return &playbackReader{queue: queue, wake: wake, demand: demand, currentOffset: FrameSamples}
}

func (r *playbackReader) Read(samples []float32) {
	clear(samples)
	for len(samples) > 0 {
		if r.currentOffset == FrameSamples {
			r.current = nil
			for {
				slot, ok := r.queue.AcquireRead()
				if !ok {
					break
				}
				if slot.Index < r.currentIndex {
					r.queue.ReleaseRead()
					continue
				}
				if slot.Index == r.currentIndex {
					r.current = slot
				}
				break
			}
			r.currentOffset = 0
			r.demand.Store(r.currentIndex + 1)
			notifyPlayback(r.wake)
		}
		count := min(len(samples), FrameSamples-r.currentOffset)
		if r.current != nil {
			copy(samples[:count], r.current.Samples[r.currentOffset:r.currentOffset+count])
		}
		r.currentOffset += count
		samples = samples[count:]
		if r.currentOffset == FrameSamples {
			if r.current != nil {
				r.queue.ReleaseRead()
				r.current = nil
			}
			r.currentIndex++
		}
	}
}
