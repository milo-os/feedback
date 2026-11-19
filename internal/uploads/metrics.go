package uploads

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Route label values.
const (
	routeUpload   = "upload"
	routeDownload = "download"
)

// Refusal reasons.
const (
	reasonReplicaBusy = "replica_busy"
	reasonUserBusy    = "user_busy"
	reasonRateLimited = "rate_limited"
)

// codes are the status codes each route answers with.
var codes = map[string][]int{
	routeUpload: {
		http.StatusCreated, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusTooManyRequests, http.StatusServiceUnavailable,
	},
	routeDownload: {
		http.StatusOK, http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable,
	},
}

// Bucket operations that can fail.
const (
	opUpload = "upload"
	opGet    = "get"
	opOpen   = "open"
)

// Metrics records what feedback-uploads does. A nil *Metrics records
// nothing.
type Metrics struct {
	requests     *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	refused      *prometheus.CounterVec
	bucketErrors *prometheus.CounterVec
	inFlight     *prometheus.GaugeVec
}

// NewMetrics registers the service's metrics with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "feedback_uploads_requests_total",
			Help: "Upload and download requests, by route and HTTP status code.",
		}, []string{"route", "code"}),
		// Transfers can take minutes (TransferTimeout), so the buckets reach
		// past three.
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "feedback_uploads_request_duration_seconds",
			Help:    "Time to answer upload and download requests, including moving the file.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2.5, 10),
		}, []string{"route"}),
		refused: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "feedback_uploads_refused_total",
			Help: "Requests refused before moving a file: replica_busy (503, every slot on the replica taken), user_busy (429, the user's own slots taken) or rate_limited (429).",
		}, []string{"route", "reason"}),
		bucketErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "feedback_uploads_bucket_errors_total",
			Help: "Bucket operations that failed, by operation: upload (storing a file), get (reading its attributes) or open (starting to read it). Missing files aren't errors.",
		}, []string{"operation"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "feedback_uploads_transfers_in_flight",
			Help: "Transfers holding a slot on this replica.",
		}, []string{"route"}),
	}
	reg.MustRegister(m.requests, m.duration, m.refused, m.bucketErrors, m.inFlight)
	// Start every series at zero, so rates and alerts see the first event.
	for _, route := range []string{routeUpload, routeDownload} {
		for _, code := range codes[route] {
			m.requests.WithLabelValues(route, strconv.Itoa(code))
		}
		m.duration.WithLabelValues(route)
		m.inFlight.WithLabelValues(route)
		m.refused.WithLabelValues(route, reasonReplicaBusy)
		m.refused.WithLabelValues(route, reasonUserBusy)
	}
	m.refused.WithLabelValues(routeUpload, reasonRateLimited)
	for _, op := range []string{opUpload, opGet, opOpen} {
		m.bucketErrors.WithLabelValues(op)
	}
	return m
}

// instrument records each request to route: its status code and how long it
// took.
func (m *Metrics) instrument(route string, h http.HandlerFunc) http.HandlerFunc {
	if m == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h(sw, r)
		code := sw.code
		if code == 0 {
			code = http.StatusOK
		}
		m.requests.WithLabelValues(route, strconv.Itoa(code)).Inc()
		m.duration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	}
}

func (m *Metrics) refuse(route, reason string) {
	if m != nil {
		m.refused.WithLabelValues(route, reason).Inc()
	}
}

func (m *Metrics) bucketError(op string) {
	if m != nil {
		m.bucketErrors.WithLabelValues(op).Inc()
	}
}

// transferring counts a transfer as in flight until the returned func is
// called.
func (m *Metrics) transferring(route string) func() {
	if m == nil {
		return func() {}
	}
	g := m.inFlight.WithLabelValues(route)
	g.Inc()
	return g.Dec
}

// statusWriter remembers the status code a handler sent. Unwrap lets
// http.ResponseController reach the connection to set transfer deadlines.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
