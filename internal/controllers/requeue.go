package controllers

import "time"

// requeueDelay is how long the controller waits before polling CertForge again
// for a pending approval. cert-manager approval workflows can take minutes for
// human review, so 15 seconds is a reasonable balance between responsiveness
// and API load.
const requeueDelay = 15 * time.Second
