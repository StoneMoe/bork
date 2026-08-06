package audio

import "sync/atomic"

type captureAssembler struct {
	queue      *pcmFrameQueue
	ready      chan<- struct{}
	generation *atomic.Uint64

	current        *pcmFrame
	frameOffset    int
	dropFrame      bool
	silentFrame    bool
	referenceValid bool
	sampleClock    uint32
}

func (a *captureAssembler) Write(samples, reference []float32, muted bool) {
	referenceLengthValid := len(reference) == len(samples)
	for len(samples) > 0 {
		if a.frameOffset == 0 {
			a.dropFrame = false
			a.current = nil
			a.current, _ = a.queue.AcquireWrite()
			a.dropFrame = a.current == nil
			a.silentFrame = muted
			a.referenceValid = true
			if a.current != nil && a.generation != nil {
				a.current.Generation = a.generation.Load()
			}
		}
		if muted {
			a.silentFrame = true
		}
		if !referenceLengthValid {
			a.referenceValid = false
		}
		count := min(len(samples), FrameSamples-a.frameOffset)
		if len(reference) < count {
			a.referenceValid = false
		}
		if !a.dropFrame {
			if !a.silentFrame {
				copy(a.current.Samples[a.frameOffset:a.frameOffset+count], samples[:count])
			}
			clear(a.current.Reference[a.frameOffset : a.frameOffset+count])
			copy(a.current.Reference[a.frameOffset:a.frameOffset+count], reference[:min(count, len(reference))])
		}
		a.frameOffset += count
		a.sampleClock += uint32(count)
		samples = samples[count:]
		reference = reference[min(count, len(reference)):]
		if a.frameOffset == FrameSamples {
			if !a.dropFrame {
				if a.silentFrame {
					clear(a.current.Samples[:])
				}
				a.current.Muted = a.silentFrame
				a.current.ReferenceValid = a.referenceValid
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
	queue         *pcmFrameQueue
	wake          chan<- struct{}
	demand        *atomic.Uint64
	playbackMuted *atomic.Bool

	current       *pcmFrame
	currentOffset int
	currentIndex  uint64
}

func newPlaybackReader(queue *pcmFrameQueue, wake chan<- struct{}, demand *atomic.Uint64, playbackMuted *atomic.Bool) *playbackReader {
	return &playbackReader{queue: queue, wake: wake, demand: demand, playbackMuted: playbackMuted, currentOffset: FrameSamples}
}

func (r *playbackReader) Read(samples []float32) {
	clear(samples)
	muted := r.playbackMuted.Load()
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
		if r.current != nil && (!muted || r.current.LocalOnly) {
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
