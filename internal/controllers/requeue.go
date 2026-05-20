package controllers

import (
	"fmt"
	"time"
)

// requeueDelay is how long the controller waits before polling CertForge again
// for a pending approval. cert-manager approval workflows can take minutes for
// human review, so 15 seconds is a reasonable balance between responsiveness
// and API load.
const requeueDelay = 15 * time.Second

func formatElapsed(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
