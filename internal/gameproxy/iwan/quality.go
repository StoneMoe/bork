//go:build game_proxy

package iwan

import "time"

const (
	linkLossWindow    = 30 * time.Second
	linkHistoryWindow = 3 * time.Minute
	maxLinkSamples    = 180
)

type LinkQuality struct {
	ObservedAt  time.Time    `json:"observedAt"`
	RTTMillis   *float64     `json:"rttMs"`
	LossPercent *float64     `json:"lossPercent"`
	History     []LinkSample `json:"history"`
}

type LinkSample struct {
	At          time.Time `json:"at"`
	Generation  uint64    `json:"generation"`
	RTTMillis   *float64  `json:"rttMs"`
	LossPercent float64   `json:"lossPercent"`
}

// Clone detaches all mutable data at status publication boundaries.
func (quality LinkQuality) Clone() LinkQuality {
	if quality.RTTMillis != nil {
		quality.RTTMillis = new(*quality.RTTMillis)
	}
	if quality.LossPercent != nil {
		quality.LossPercent = new(*quality.LossPercent)
	}
	history := make([]LinkSample, len(quality.History))
	copy(history, quality.History)
	for index := range history {
		if history[index].RTTMillis != nil {
			history[index].RTTMillis = new(*history[index].RTTMillis)
		}
	}
	quality.History = history
	return quality
}

// The supervisor mutex protects the sole history across all generations.
func (supervisor *Supervisor) recordLinkSample(id uint64, at time.Time, rtt *float64) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.desired || supervisor.status.State != StateReady || supervisor.status.Generation != id {
		return
	}
	supervisor.pruneLinkHistoryLocked(at)
	if len(supervisor.linkHistory) == maxLinkSamples {
		copy(supervisor.linkHistory, supervisor.linkHistory[1:])
		supervisor.linkHistory = supervisor.linkHistory[:maxLinkSamples-1]
	}
	if rtt != nil {
		rtt = new(*rtt)
	}
	supervisor.linkHistory = append(supervisor.linkHistory, LinkSample{At: at, Generation: id, RTTMillis: rtt})
	supervisor.linkHistory[len(supervisor.linkHistory)-1].LossPercent = *supervisor.linkLossLocked(id, at)
}

func (supervisor *Supervisor) pruneLinkHistoryLocked(now time.Time) {
	first := 0
	for first < len(supervisor.linkHistory) && now.Sub(supervisor.linkHistory[first].At) >= linkHistoryWindow {
		first++
	}
	if first > 0 {
		remaining := copy(supervisor.linkHistory, supervisor.linkHistory[first:])
		clear(supervisor.linkHistory[remaining:])
		supervisor.linkHistory = supervisor.linkHistory[:remaining]
	}
}

func (supervisor *Supervisor) linkLossLocked(id uint64, now time.Time) *float64 {
	completed, lost := 0, 0
	for _, sample := range supervisor.linkHistory {
		age := now.Sub(sample.At)
		if sample.Generation != id || age < 0 || age >= linkLossWindow {
			continue
		}
		completed++
		if sample.RTTMillis == nil {
			lost++
		}
	}
	if completed == 0 {
		return nil
	}
	return new(100 * float64(lost) / float64(completed))
}

func (supervisor *Supervisor) linkQualityLocked(now time.Time) LinkQuality {
	supervisor.pruneLinkHistoryLocked(now)
	quality := LinkQuality{ObservedAt: now, History: supervisor.linkHistory}
	if supervisor.desired && supervisor.status.State == StateReady {
		quality.LossPercent = supervisor.linkLossLocked(supervisor.status.Generation, now)
		if quality.LossPercent != nil {
			// The latest outcome, including a timeout, replaces the current RTT.
			quality.RTTMillis = supervisor.linkHistory[len(supervisor.linkHistory)-1].RTTMillis
		}
	}
	return quality.Clone()
}
