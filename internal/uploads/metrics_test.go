package uploads

import (
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"go.miloapis.com/feedback/internal/ratelimit"
)

// Rates and alerts only see an event if its series existed before it, so
// every series must be there, at zero, before the first request.
func TestMetricsStartAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	requests := map[string]float64{}
	durations := map[string]uint64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			switch f.GetName() {
			case "feedback_uploads_requests_total":
				requests[labels["route"]+" "+labels["code"]] = m.GetCounter().GetValue()
			case "feedback_uploads_request_duration_seconds":
				durations[labels["route"]] = m.GetHistogram().GetSampleCount()
			}
		}
	}
	for _, route := range []string{routeUpload, routeDownload} {
		if n, ok := durations[route]; !ok || n != 0 {
			t.Errorf("%s duration: present %v, count %d, want present at 0", route, ok, n)
		}
		for _, code := range codes[route] {
			key := route + " " + strconv.Itoa(code)
			if v, ok := requests[key]; !ok || v != 0 {
				t.Errorf("%s requests: present %v, value %v, want present at 0", key, ok, v)
			}
		}
	}
	for _, key := range []string{"upload 201", "upload 429", "upload 503", "download 200", "download 404", "download 503"} {
		if _, ok := requests[key]; !ok {
			t.Errorf("%s requests: missing", key)
		}
	}
}

func TestMetricsCountRequestsAndRefusals(t *testing.T) {
	s, h := newServer(t)
	m := s.Metrics
	s.Uploads, s.UploadsPerUser = NewSlots(4), NewUserSlots(2)
	s.Downloads = NewSlots(1)
	s.Limiter = ratelimit.NewLimiter(60, 2)
	upload := func(name string) int {
		return do(h, http.MethodPost, "/users/u1/uploads/submissions?name="+name, "ada", "text/plain", []byte("hello")).Code
	}

	ref := uploadRef(t, do(h, http.MethodPost, "/users/u1/uploads/submissions?name=a.txt", "ada", "text/plain", []byte("hello")))
	attach(t, s, "sub-1", "u1", ref)
	if code := do(h, http.MethodGet, "/users/u1/submissions/sub-1/attachments/"+ref, "ada", "", nil).Code; code != http.StatusOK {
		t.Fatalf("download: %d", code)
	}
	if code := upload("b.txt"); code != http.StatusCreated {
		t.Fatalf("second upload: %d", code)
	}
	// The limiter's burst of 2 is spent.
	if code := upload("c.txt"); code != http.StatusTooManyRequests {
		t.Fatalf("upload past the rate limit: %d", code)
	}
	// Ada's own slots, then the replica's download slot, are taken.
	s.UploadsPerUser.take("ada@example.com")
	s.UploadsPerUser.take("ada@example.com")
	if code := upload("d.txt"); code != http.StatusTooManyRequests {
		t.Fatalf("upload at Ada's cap: %d", code)
	}
	s.Downloads.take()
	if code := do(h, http.MethodGet, "/users/u1/submissions/sub-1/attachments/"+ref, "ada", "", nil).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("download on a full replica: %d", code)
	}
	s.Downloads.give()
	// A missing file is a 404, not a bucket error.
	attach(t, s, "sub-2", "u1", "att-0000000000000000000000ff")
	if code := do(h, http.MethodGet, "/users/u1/submissions/sub-2/attachments/att-0000000000000000000000ff", "ada", "", nil).Code; code != http.StatusNotFound {
		t.Fatalf("download of a missing file: %d", code)
	}

	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"uploads 201", testutil.ToFloat64(m.requests.WithLabelValues(routeUpload, "201")), 2},
		{"uploads 429", testutil.ToFloat64(m.requests.WithLabelValues(routeUpload, "429")), 2},
		{"downloads 200", testutil.ToFloat64(m.requests.WithLabelValues(routeDownload, "200")), 1},
		{"downloads 503", testutil.ToFloat64(m.requests.WithLabelValues(routeDownload, "503")), 1},
		{"downloads 404", testutil.ToFloat64(m.requests.WithLabelValues(routeDownload, "404")), 1},
		{"rate limited", testutil.ToFloat64(m.refused.WithLabelValues(routeUpload, reasonRateLimited)), 1},
		{"user busy", testutil.ToFloat64(m.refused.WithLabelValues(routeUpload, reasonUserBusy)), 1},
		{"replica busy", testutil.ToFloat64(m.refused.WithLabelValues(routeDownload, reasonReplicaBusy)), 1},
		{"bucket get errors", testutil.ToFloat64(m.bucketErrors.WithLabelValues(opGet)), 0},
		{"uploads in flight", testutil.ToFloat64(m.inFlight.WithLabelValues(routeUpload)), 0},
		{"downloads in flight", testutil.ToFloat64(m.inFlight.WithLabelValues(routeDownload)), 0},
	} {
		if c.got != c.want {
			t.Errorf("%s: %v, want %v", c.name, c.got, c.want)
		}
	}
	if n := testutil.CollectAndCount(m.duration); n != 2 {
		t.Errorf("duration series: %d, want one per route", n)
	}
}

func TestMetricsCountBucketErrors(t *testing.T) {
	dir := t.TempDir()
	s, h := newServerAt(t, "file://"+dir, 1024)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if code := do(h, http.MethodPost, "/users/u1/uploads/submissions?name=a.txt", "ada", "text/plain", []byte("hello")).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("upload to a read-only bucket: %d", code)
	}
	if got := testutil.ToFloat64(s.Metrics.bucketErrors.WithLabelValues(opUpload)); got != 1 {
		t.Errorf("bucket upload errors: %v, want 1", got)
	}
}
