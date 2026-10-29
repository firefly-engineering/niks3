package server_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mic92/niks3/api"
	"github.com/Mic92/niks3/server"
	"github.com/Mic92/niks3/server/signing"
	minio "github.com/minio/minio-go/v7"
)

// newTestKey returns a signing key and its public half.
func newTestKey(tb testing.TB, name string) (*signing.Key, *signing.PublicKey) {
	tb.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	ok(tb, err)

	key, err := signing.ParseKey(name + ":" + base64.StdEncoding.EncodeToString(priv))
	ok(tb, err)

	pubStr, err := key.PublicKey()
	ok(tb, err)

	pub, err := signing.ParsePublicKey(pubStr)
	ok(tb, err)

	return key, pub
}

// fakeUpstream is a binary cache serving fixed objects and counting requests.
type fakeUpstream struct {
	*httptest.Server

	mu       sync.Mutex
	objects  map[string][]byte
	requests map[string]int
}

func newFakeUpstream(tb testing.TB) *fakeUpstream {
	tb.Helper()

	u := &fakeUpstream{objects: map[string][]byte{}, requests: map[string]int{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests[r.URL.Path]++
		body, found := u.objects[r.URL.Path]
		u.mu.Unlock()

		if !found {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(body)
	}))
	tb.Cleanup(u.Close)

	return u
}

func (u *fakeUpstream) hits(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.requests[path]
}

// upstreamPath is a store path an upstream holds. narURL is the NAR's name
// on the upstream, narKey the one niks3 stores it under.
type upstreamPath struct {
	hash, narURL, narKey string
	nar                  []byte
}

// addPath publishes a zstd NAR with content and its narinfo signed by key.
// tamper replaces the NAR bytes after signing.
func (u *fakeUpstream) addPath(tb testing.TB, key *signing.Key, content string, tamper bool) upstreamPath {
	tb.Helper()

	digest := sha256.Sum256([]byte(content))
	hash := api.EncodeNixBase32(digest[:20])
	info := &signing.NarInfo{
		StorePath:  "/nix/store/" + hash + "-upstream-test",
		NarHash:    "sha256:" + api.EncodeNixBase32(digest[:]),
		NarSize:    uint64(len(content)),
		References: []string{"/nix/store/" + hash + "-upstream-test"},
	}

	sigs, err := key.Sign(tb.Context(), map[string]*signing.NarInfo{hash: info})
	ok(tb, err)

	nar := zstdCompress(tb, []byte(content))
	if tamper {
		nar = zstdCompress(tb, []byte(strings.ToUpper(content))) // same size, other hash
	}

	// Named like cachix does, not by hash.
	narURL := "nar/" + hash + "-0000-4000-8000-000000000000.nar.zst"

	narinfo := fmt.Sprintf("StorePath: %s\nURL: %s\nCompression: zstd\nFileSize: %d\nNarHash: %s\nNarSize: %d\nReferences: %s\nSig: %s\n",
		info.StorePath, narURL, len(nar), info.NarHash, info.NarSize, hash+"-upstream-test", sigs[hash])

	u.mu.Lock()
	u.objects["/"+hash+".narinfo"] = []byte(narinfo)
	u.objects["/"+narURL] = nar
	u.mu.Unlock()

	narKey := "nar/" + strings.TrimPrefix(info.NarHash, "sha256:") + ".nar.zst"

	return upstreamPath{hash: hash, narURL: narURL, narKey: narKey, nar: nar}
}

// createUpstreamTestService returns a read-proxy service filling from
// upstream, and niks3's own public key.
func createUpstreamTestService(t *testing.T, upstream *fakeUpstream, upstreamKey *signing.PublicKey) (*server.Service, *signing.PublicKey) {
	t.Helper()

	niks3Key, niks3Pub := newTestKey(t, "niks3-test-1")

	service := createProxyTestService(t)
	service.SigningKeys = []signing.Signer{niks3Key}
	service.Upstreams = []server.Upstream{{URL: upstream.URL, Key: upstreamKey}}
	service.ReadsGatedByProxy = true

	t.Cleanup(service.Close)

	return service, niks3Pub
}

func objectExists(t *testing.T, service *server.Service, key string) bool {
	t.Helper()

	_, err := service.MinioClient.StatObject(t.Context(), service.Bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true
	}

	if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
		return false
	}

	t.Fatalf("stat %s: %v", key, err)

	return false
}

func TestUpstreamFillResignsAndServes(t *testing.T) {
	t.Parallel()

	upstreamKey, upstreamPub := newTestKey(t, "upstream-1")
	upstream := newFakeUpstream(t)
	path := upstream.addPath(t, upstreamKey, "hello from upstream", false)

	service, niks3Pub := createUpstreamTestService(t, upstream, upstreamPub)
	ts := setupProxyServer(t, service)
	defer ts.Close()

	// Concurrent misses for one path fill once.
	var wg sync.WaitGroup

	bodies := make([][]byte, 5)
	for i := range bodies {
		wg.Go(func() { _, bodies[i] = proxyGet(t, ts, "/"+path.hash+".narinfo", http.StatusOK) })
	}

	wg.Wait()

	if n := upstream.hits("/" + path.narURL); n != 1 {
		t.Errorf("upstream NAR fetched %d times, want 1", n)
	}

	got, err := server.ParseNarinfoForTest(string(bodies[0]))
	ok(t, err)

	if !niks3Pub.VerifyNarinfo(&got.NarInfo, got.Sigs) {
		t.Errorf("served narinfo lacks a valid niks3 signature:\n%s", bodies[0])
	}

	if got.URL != path.narKey {
		t.Errorf("URL = %q, want %q", got.URL, path.narKey)
	}

	_, nar := proxyGet(t, ts, "/"+path.narKey, http.StatusOK)
	if string(nar) != string(path.nar) {
		t.Error("served NAR differs from upstream's")
	}

	// GC treats the pulled path like a pushed closure: kept while its
	// closure is young, collected once it ages out.
	if status := service.RunGCForTest(time.Hour, time.Hour, true); status.State != api.GCTaskStateSucceeded {
		t.Fatalf("GC failed: %+v", status)
	}

	if !objectExists(t, service, path.hash+".narinfo") || !objectExists(t, service, path.narKey) {
		t.Fatal("GC collected a fresh pulled path")
	}

	if status := service.RunGCForTest(0, 0, true); status.State != api.GCTaskStateSucceeded {
		t.Fatalf("GC failed: %+v", status)
	}

	if objectExists(t, service, path.hash+".narinfo") || objectExists(t, service, path.narKey) {
		t.Error("GC kept an expired pulled path")
	}
}

func TestUpstreamFillRefusals(t *testing.T) {
	t.Parallel()

	upstreamKey, upstreamPub := newTestKey(t, "upstream-1")
	otherKey, _ := newTestKey(t, "upstream-1") // same name, different key

	tests := []struct {
		name  string
		setup func(*fakeUpstream, *server.Service) upstreamPath
	}{
		{"bad signature", func(u *fakeUpstream, _ *server.Service) upstreamPath {
			return u.addPath(t, otherKey, "forged", false)
		}},
		{"NAR hash mismatch", func(u *fakeUpstream, _ *server.Service) upstreamPath {
			return u.addPath(t, upstreamKey, "tampered", true)
		}},
		{"over max NAR size", func(u *fakeUpstream, s *server.Service) upstreamPath {
			s.MaxNarSize = 4

			return u.addPath(t, upstreamKey, "too big", false)
		}},
		{"anonymous read", func(u *fakeUpstream, s *server.Service) upstreamPath {
			s.ReadsGatedByProxy = false

			return u.addPath(t, upstreamKey, "anonymous", false)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := newFakeUpstream(t)
			service, _ := createUpstreamTestService(t, upstream, upstreamPub)
			path := tc.setup(upstream, service)

			ts := setupProxyServer(t, service)
			defer ts.Close()

			proxyGet(t, ts, "/"+path.hash+".narinfo", http.StatusNotFound)

			if objectExists(t, service, path.hash+".narinfo") || objectExists(t, service, path.narKey) {
				t.Error("refused path was stored")
			}
		})
	}
}

func TestUpstreamMissIsCached(t *testing.T) {
	t.Parallel()

	_, upstreamPub := newTestKey(t, "upstream-1")
	upstream := newFakeUpstream(t)
	service, _ := createUpstreamTestService(t, upstream, upstreamPub)

	ts := setupProxyServer(t, service)
	defer ts.Close()

	hash := strings.Repeat("0", 32)
	for range 3 {
		proxyGet(t, ts, "/"+hash+".narinfo", http.StatusNotFound)
	}

	if n := upstream.hits("/" + hash + ".narinfo"); n != 1 {
		t.Errorf("upstream asked %d times for an absent path, want 1", n)
	}
}
