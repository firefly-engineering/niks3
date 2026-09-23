package server_test

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mic92/niks3/api"
	"github.com/Mic92/niks3/server"
	minio "github.com/minio/minio-go/v7"
)

// googleEndpoint is the one host minio-go treats as Google Cloud Storage
// (s3utils.IsGoogleEndpoint). On it, RemoveObjects issues one DELETE per key
// instead of a multi-object delete, and yields no result for a key that is
// already absent.
const googleEndpoint = "storage.googleapis.com"

// googleEndpointTransport lets a minio client configured for GCS talk to the
// test RustFS: every request is sent to RustFS, with the Host header (which
// SigV4 signs) left as the Google endpoint.
//
// It also answers the way GCS does where RustFS differs: a DELETE of an
// absent object is 404 NoSuchKey on GCS, while RustFS follows S3 and answers
// 204. To tell the two apart it tracks which keys have been PUT through it
// and not yet deleted, so objects must be created through this client.
type googleEndpointTransport struct {
	target string
	bucket string

	mu      sync.Mutex
	present map[string]bool
	deletes map[string]int
}

func (t *googleEndpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key, isObject := strings.CutPrefix(req.URL.Path, "/"+t.bucket+"/")

	if isObject && req.Method == http.MethodDelete {
		t.mu.Lock()
		t.deletes[key]++
		wasPresent := t.present[key]
		delete(t.present, key)
		t.mu.Unlock()

		if !wasPresent {
			return noSuchKeyResponse(req, key), nil
		}
	}

	forwarded := req.Clone(req.Context())
	forwarded.Host = req.URL.Host
	forwarded.URL.Host = t.target

	resp, err := http.DefaultTransport.RoundTrip(forwarded)
	if err != nil {
		return nil, fmt.Errorf("forwarding to rustfs: %w", err)
	}

	if isObject && req.Method == http.MethodPut && resp.StatusCode == http.StatusOK {
		t.mu.Lock()
		t.present[key] = true
		t.mu.Unlock()
	}

	return resp, nil
}

func noSuchKeyResponse(req *http.Request, key string) *http.Response {
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>` +
		`<Details>No such object: ` + key + `</Details></Error>`

	return &http.Response{
		Status:        "404 Not Found",
		StatusCode:    http.StatusNotFound,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/xml; charset=UTF-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func (t *googleEndpointTransport) deleteCounts() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return maps.Clone(t.deletes)
}

// useGoogleEndpoint points the service's S3 client at RustFS through a client
// that minio-go believes is talking to GCS. Seed objects after calling it.
func useGoogleEndpoint(tb testing.TB, service *server.Service) *googleEndpointTransport {
	tb.Helper()

	transport := &googleEndpointTransport{
		target:  fmt.Sprintf("localhost:%d", testRustfsServer.port),
		bucket:  service.Bucket,
		present: map[string]bool{},
		deletes: map[string]int{},
	}

	client, err := minio.New(googleEndpoint, &minio.Options{
		Creds:        testRustfsServer.Creds(),
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		Transport:    transport,
	})
	ok(tb, err)

	service.MinioClient = client

	return transport
}

// runGCWithDeadline runs a forced GC and fails the test if it has not
// finished by the deadline. The GC goroutine is leaked on failure: the
// cleanup loop it would be stuck in takes no context that could stop it.
func runGCWithDeadline(tb testing.TB, service *server.Service, deadline time.Duration) api.GCTaskStatus {
	tb.Helper()

	done := make(chan api.GCTaskStatus, 1)

	go func() { done <- service.RunGCForTest(0, 0, true) }()

	select {
	case status := <-done:
		return status
	case <-time.After(deadline):
		snap, _ := service.GCTasks.Get()
		tb.Fatalf("gc did not finish within %s: state %s, phase %s, stats %+v",
			deadline, snap.State, snap.Phase, snap.Stats)

		return api.GCTaskStatus{}
	}
}

func countObjectRows(tb testing.TB, service *server.Service) int {
	tb.Helper()

	var n int

	err := service.Pool.QueryRow(tb.Context(), "SELECT count(*) FROM objects").Scan(&n)
	ok(tb, err)

	return n
}

// TestGCTerminatesAgainstGoogleEndpoint is the reproduction from the GCS
// report: a handful of orphans, a forced GC, single-object DELETEs. Before
// keyset pagination the feeder handed the same keys out again before their
// rows were flushed, the second DELETE of each answered NoSuchKey, minio-go
// yielded nothing for it, and the cleanup phase never finished.
func TestGCTerminatesAgainstGoogleEndpoint(t *testing.T) {
	t.Parallel()

	service := createTestService(t)
	defer service.Close()

	orphans := make([]struct {
		key  string
		refs []string
	}, 9)
	for i := range orphans {
		orphans[i].key = fmt.Sprintf("%032d.narinfo", i)
		orphans[i].refs = []string{}
	}

	transport := useGoogleEndpoint(t, service)

	createOrphanedObjects(t, service, orphans)

	status := runGCWithDeadline(t, service, 30*time.Second)

	if status.State != api.GCTaskStateSucceeded {
		t.Fatalf("gc ended in state %s: %s", status.State, status.Error)
	}

	if status.Stats.ObjectsDeletedAfterGracePeriod != len(orphans) || status.Stats.ObjectsFailedToDelete != 0 {
		t.Errorf("expected %d deleted and 0 failed, got %d deleted and %d failed",
			len(orphans), status.Stats.ObjectsDeletedAfterGracePeriod, status.Stats.ObjectsFailedToDelete)
	}

	counts := transport.deleteCounts()
	for _, o := range orphans {
		if counts[o.key] != 1 {
			t.Errorf("key %s: expected 1 DELETE, got %d", o.key, counts[o.key])
		}
	}

	if n := countObjectRows(t, service); n != 0 {
		t.Errorf("expected no object rows left, got %d", n)
	}
}
