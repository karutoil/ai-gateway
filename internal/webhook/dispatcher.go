package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Dispatcher is the Phase 2.5 webhook scaffold. Phase 3 will fan out to multiple URLs.
type Dispatcher interface {
	Emit(event string, payload any)
}

// EmptyDispatcher discards events when WEBHOOK_URL not set
type EmptyDispatcher struct{}

func (n *EmptyDispatcher) Emit(_ string, _ any) {}

var _ Dispatcher = (*EmptyDispatcher)(nil)

// WebhookTimeout bounds each delivery attempt.
const WebhookTimeout = 5 * time.Second

// HTTPDispatcher POSTs JSON to WEBHOOK_URL asynchronously: Emit enqueues onto a
// bounded channel consumed by one background worker, so a slow or wedged sink
// can never add latency to request admission (/v1 quota 429s) or admin
// mutations (audit trail). Deliveries get a bounded retry with backoff;
// overflow drops oldest-with-log rather than blocking producers.
type HTTPDispatcher struct {
	URL    string
	Client *http.Client
	// Secret, when non-empty, signs deliveries:
	// X-Webhook-Signature: sha256=<hex HMAC-SHA256 of the raw body> — lets
	// consumers verify authenticity instead of trusting any caller that
	// knows the URL.
	Secret string

	queue    chan queuedEvent
	stop     chan struct{}
	done     chan struct{}
	started  sync.Once
	stopOnce sync.Once
}

type queuedEvent struct {
	event string
	body  []byte
}

// QueueDepth bounds the number of pending deliveries.
const QueueDepth = 256

var _ Dispatcher = (*HTTPDispatcher)(nil)

// NewFromEnv returns HTTPDispatcher when WEBHOOK_URL is set, otherwise EmptyDispatcher.
func NewFromEnv() Dispatcher {
	url := os.Getenv("WEBHOOK_URL")
	if url == "" {
		return &EmptyDispatcher{}
	}
	return NewSigned(url, os.Getenv("WEBHOOK_SECRET"))
}

// New returns dispatcher based on explicit URL (empty => Empty).
func New(url string) Dispatcher {
	return NewSigned(url, "")
}

// NewSigned is New with optional HMAC signing secret (WEBHOOK_SECRET).
func NewSigned(url, secret string) Dispatcher {
	if url == "" {
		return &EmptyDispatcher{}
	}
	return &HTTPDispatcher{
		URL:    url,
		Secret: secret,
		Client: &http.Client{Timeout: WebhookTimeout},
		queue:  make(chan queuedEvent, QueueDepth),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

func marshalEvent(event string, payload any) []byte {
	body, err := json.Marshal(map[string]any{
		"event":   event,
		"payload": payload,
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		log.Error().Err(err).Str("event", event).Msg("webhook marshal failed")
		return nil
	}
	return body
}

// Emit enqueues for asynchronous delivery; it never blocks on the sink.
func (h *HTTPDispatcher) Emit(event string, payload any) {
	h.started.Do(func() { go h.worker() })
	body := marshalEvent(event, payload)
	if body == nil {
		return
	}
	select {
	case h.queue <- queuedEvent{event: event, body: body}:
	default:
		// Drop the OLDEST entry to admit the newest (audit events are append-most).
		select {
		case <-h.queue:
			log.Warn().Str("event", event).Msg("webhook queue full, dropped oldest")
		default:
		}
		select {
		case h.queue <- queuedEvent{event: event, body: body}:
		default:
			log.Warn().Str("event", event).Msg("webhook queue still full, dropping")
		}
	}
}

// worker drains the queue. Two attempts with backoff keep delivery best-effort
// without ever stalling callers. On Stop the remaining backlog is flushed with
// a single best-effort attempt per event so queued audit events are not
// silently lost at shutdown.
func (h *HTTPDispatcher) worker() {
	defer close(h.done)
	for {
		select {
		case ev := <-h.queue:
			if !h.deliver(ev.event, ev.body) {
				time.Sleep(500 * time.Millisecond)
				h.deliver(ev.event, ev.body)
			}
		case <-h.stop:
			for {
				select {
				case ev := <-h.queue:
					if !h.deliver(ev.event, ev.body) {
						h.deliver(ev.event, ev.body) // final attempt, no backoff sleep on shutdown
					}
				default:
					return
				}
			}
		}
	}
}

// Stop halts the background worker after draining already-queued events and
// waits for its exit (each pending delivery is bounded by WebhookTimeout).
// Safe to call repeatedly or concurrently with Emit; events emitted after Stop
// are enqueued but no worker remains to deliver them. Exists so embedders and
// tests can shut down without leaking the worker goroutine.
func (h *HTTPDispatcher) Stop() {
	h.started.Do(func() { go h.worker() }) // ensure a worker exists to observe the stop
	h.stopOnce.Do(func() { close(h.stop) })
	<-h.done
}

func (h *HTTPDispatcher) deliver(event string, body []byte) bool {
	req, err := http.NewRequest(http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		log.Error().Err(err).Str("event", event).Msg("webhook request build failed")
		return true // unbuildable requests will never succeed
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Event", event)
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		req.Header.Set("X-Webhook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		log.Error().Err(err).Str("event", event).Str("url", h.URL).Msg("webhook emit failed")
		return false
	}
	defer resp.Body.Close()
	log.Info().Str("event", event).Int("status", resp.StatusCode).Str("url", h.URL).Msg("webhook emitted")
	return true
}

// Global dispatcher used by audit recorder; swapped in tests.
var Global Dispatcher = &EmptyDispatcher{}

func init() {
	ReinitFromEnv()
}

// ReinitFromEnv (re)creates Global from the environment. init() runs BEFORE
// config.Load() loads .env files, so WEBHOOK_URL set only via .env was
// silently ignored — production wiring calls this again after config.Load.
func ReinitFromEnv() {
	if url := os.Getenv("WEBHOOK_URL"); url != "" {
		Global = NewSigned(url, os.Getenv("WEBHOOK_SECRET"))
	}
}
