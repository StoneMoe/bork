package screenshare

const (
	SourceMonitor = "monitor"
	SourceWindow  = "window"
)

// Source describes one native screen or window that can be shared. ID belongs
// to this list snapshot and may become stale when a window or display closes.
type Source struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Sources returns the capture targets currently available on this computer.
func Sources() ([]Source, error) {
	return listSources()
}
