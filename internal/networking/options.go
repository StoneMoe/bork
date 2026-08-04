package networking

import "bork/internal/networking/endpoint"

type Options struct {
	Endpoint          endpoint.Options
	TrackerURLs       []string
	EnablePortMapping bool
}
