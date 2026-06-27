package chat

import (
	"net/http"
	"sync"
)

// chatTransportMaxIdleConnsPerHost bounds the keep-alive connections
// pooled per upstream host for vornik's HTTP chat providers.
//
// Go's default (http.DefaultTransport) keeps only 2 idle connections per
// host (DefaultMaxIdleConnsPerHost). With the chat queue's default of 8
// concurrent calls (chat.max_concurrent_requests, see
// container_chat.go), that forces ~6 of every 8 simultaneous calls to a
// single host to open a fresh TCP+TLS connection and drop it after use —
// a TLS-handshake tail latency on the hot path. It is NOT a hard
// parallelism cap: DefaultTransport leaves MaxConnsPerHost at 0
// (unlimited). We gate parallelism at the queue layer (QueuedProvider),
// not the transport; here we only fix the under-sized idle pool.
//
// 32 covers queue=8 with headroom for the case where many logical routes
// share ONE host: OpenRouter serves every model through openrouter.ai,
// so a queue-full burst (plus the router's per-route queues,
// route_queue.go) can land well more than 8 concurrent calls on a single
// host. 32 ≥ any realistic same-host concurrency on this deployment.
const chatTransportMaxIdleConnsPerHost = 32

// newChatTransport returns a fresh *http.Transport cloned from
// http.DefaultTransport with the chat connection-pool tuning applied. It
// is separate from sharedHTTPTransport so tests can inject a counting
// DialContext and assert the pool actually reuses connections.
func newChatTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = chatTransportMaxIdleConnsPerHost
	t.MaxConnsPerHost = 0 // no limit — the chat queue is the parallelism gate
	return t
}

// sharedHTTPTransport returns the process-wide connection pool for
// vornik's HTTP chat providers: the OpenAI-compat/OpenRouter client
// (client.go), the streaming client (stream.go), and the Claude/Codex
// subscription clients. One shared pool maximises keep-alive reuse
// across providers and routes that hit the same host; per-provider
// transports would fragment the pool and re-introduce the churn this
// fixes. The Bedrock path is unaffected — it uses the AWS SDK's own
// transport.
var sharedHTTPTransport = sync.OnceValue(newChatTransport)
