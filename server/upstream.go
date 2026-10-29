package server

import (
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mic92/niks3/api"
	"github.com/Mic92/niks3/server/signing"
	"github.com/klauspost/compress/zstd"
	minio "github.com/minio/minio-go/v7"
)

const (
	// upstreamMissTTL is how long a path no upstream has stays a known miss,
	// so a client retrying an absent path doesn't hit every upstream each time.
	upstreamMissTTL = time.Minute

	// shortcut: one fixed budget for any NAR size; derive it from NarSize
	// (like ProxyWriteTimeout) if fills of huge NARs over slow links time out.
	upstreamFillTimeout = 30 * time.Minute

	// maxUpstreamNarinfoSize bounds an upstream narinfo. Real ones are a few
	// kB even with thousands of references.
	maxUpstreamNarinfoSize = 1 << 20

	// upstreamPartSize keeps PutObject's buffer small for NARs of unknown
	// compressed size (minio would otherwise size parts for a 5 TiB object).
	upstreamPartSize = 16 << 20

	nixStoreDir = "/nix/store/"

	compressionNone  = "none"
	compressionZstd  = "zstd"
	compressionBzip2 = "bzip2"
)

var (
	errNoUpstreamSigningKey = errors.New("upstream caches need at least one --sign-key-path to re-sign pulled paths")
	errUpstreamMiss         = errors.New("upstream does not have the path")
)

// Upstream is a binary cache niks3 fills misses from. Its narinfos must
// carry a valid signature by Key.
type Upstream struct {
	URL string
	Key *signing.PublicKey
}

// upstreamsFlag is a repeatable --upstream-cache flag.
type upstreamsFlag []Upstream

func (f *upstreamsFlag) String() string {
	urls := make([]string, len(*f))
	for i, u := range *f {
		urls[i] = u.URL
	}

	return strings.Join(urls, ", ")
}

func (f *upstreamsFlag) Set(value string) error {
	upstream, err := ParseUpstream(value)
	if err != nil {
		return err
	}

	*f = append(*f, upstream)

	return nil
}

// ParseUpstream parses "<cache-url> <public-key>", e.g.
// "https://example.cachix.org example.cachix.org-1:base64key".
func ParseUpstream(s string) (Upstream, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return Upstream{}, fmt.Errorf("upstream %q: expected \"<cache-url> <public-key>\"", s)
	}

	u, err := url.Parse(fields[0])
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != schemeHTTPS) {
		return Upstream{}, fmt.Errorf("upstream %q: invalid cache URL", s)
	}

	key, err := signing.ParsePublicKey(fields[1])
	if err != nil {
		return Upstream{}, fmt.Errorf("upstream %q: %w", s, err)
	}

	return Upstream{URL: strings.TrimSuffix(u.String(), "/"), Key: key}, nil
}

// configureUpstreams takes the upstream options, refusing a configuration
// where fills could not be re-signed, or where anonymous reads could make the
// cache grow. It needs the signing keys and OIDC validator in place.
func (s *Service) configureUpstreams(opts *options) error {
	s.Upstreams = opts.Upstreams
	s.ReadsGatedByProxy = opts.ReadsGatedByProxy

	if len(s.Upstreams) == 0 {
		return nil
	}

	if len(s.SigningKeys) == 0 {
		return errNoUpstreamSigningKey
	}

	if !s.readGated() && !s.ReadsGatedByProxy {
		return errors.New("--upstream-cache needs authenticated reads: configure a read rule " +
			"(--mtls-bound-subject-read or an OIDC read scope), or set --reads-gated-by-proxy")
	}

	return nil
}

// upstreamMisses remembers paths no upstream had, until their entry expires.
type upstreamMisses struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (m *upstreamMisses) recent(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return time.Now().Before(m.until[hash])
}

func (m *upstreamMisses) record(hash string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	if m.until == nil {
		m.until = make(map[string]time.Time)
	}

	// Fills only run for authenticated reads, so the map grows with what
	// trusted clients ask for; sweeping expired entries keeps it at that.
	if len(m.until) >= 10_000 {
		for k, t := range m.until {
			if now.After(t) {
				delete(m.until, k)
			}
		}
	}

	m.until[hash] = now.Add(upstreamMissTTL)
}

// mayFillFromUpstream reports whether r may pull a missing path from an
// upstream: niks3 authenticated the reader, or the operator asserted a
// proxy in front authenticates every read. Anonymous reads never fill.
func (s *Service) mayFillFromUpstream(r *http.Request) bool {
	if len(s.Upstreams) == 0 {
		return false
	}

	if s.ReadsGatedByProxy {
		return true
	}

	_, ok := r.Context().Value(principalKey{}).(principal)

	return ok
}

// fillFromUpstream makes sure narinfoKey is in the bucket, pulling it from
// the first upstream that has a validly signed copy. Concurrent calls for
// one path share a single fill. A path no upstream has is not an error.
func (s *Service) fillFromUpstream(ctx context.Context, narinfoKey string) error {
	exists, err := s.objectExistsInS3(ctx, narinfoKey)
	if err != nil || exists {
		return err
	}

	hash := strings.TrimSuffix(narinfoKey, ".narinfo")

	// The fill outlives a client that gives up: the next request finds it done.
	_, err, _ = s.upstreamFills.Do(hash, func() (any, error) {
		fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamFillTimeout)
		defer cancel()

		return nil, s.fillOnce(fillCtx, hash)
	})

	return err //nolint:wrapcheck // already wrapped inside fillOnce
}

func (s *Service) fillOnce(ctx context.Context, hash string) error {
	if s.upstreamMisses.recent(hash) {
		return nil
	}

	for _, upstream := range s.Upstreams {
		info, err := fetchUpstreamNarinfo(ctx, upstream, hash, s.MaxNarSize)
		if errors.Is(err, errUpstreamMiss) {
			continue
		}

		if err != nil {
			slog.Warn("Refusing upstream narinfo", "upstream", upstream.URL, "hash", hash, "error", err)

			continue
		}

		if err := s.storeUpstreamPath(ctx, upstream, info); err != nil {
			return fmt.Errorf("filling %s from %s: %w", hash, upstream.URL, err)
		}

		slog.Info("Filled path from upstream", "upstream", upstream.URL, "store_path", info.StorePath)

		return nil
	}

	s.upstreamMisses.record(hash)

	return nil
}

// upstreamNarinfo is the part of an upstream narinfo niks3 keeps.
type upstreamNarinfo struct {
	signing.NarInfo

	// URL is the NAR's location on the upstream; NarKey is where niks3
	// stores it, named by NarHash like a pushed NAR.
	URL         string
	NarKey      string
	Compression string
	Deriver     string
	CA          string
	Sigs        []string
}

// fetchUpstreamNarinfo returns hash's narinfo from upstream, or
// errUpstreamMiss if the upstream doesn't have it. A narinfo that fails any
// check is another error.
func fetchUpstreamNarinfo(ctx context.Context, upstream Upstream, hash string, maxNarSize uint64) (*upstreamNarinfo, error) {
	//nolint:gosec // G704: upstream URL is operator config, hash is narrowed to narinfoRe
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/"+hash+".narinfo", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return nil, fmt.Errorf("fetching narinfo: %w", err)
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusForbidden: // S3-backed caches answer 403 for absent keys
		return nil, errUpstreamMiss
	default:
		return nil, fmt.Errorf("fetching narinfo: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamNarinfoSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading narinfo: %w", err)
	}

	if len(body) > maxUpstreamNarinfoSize {
		return nil, fmt.Errorf("narinfo exceeds %d bytes", maxUpstreamNarinfoSize)
	}

	info, err := parseNarinfo(string(body))
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(info.StorePath, nixStoreDir+hash+"-") || !isStorePathBase(strings.TrimPrefix(info.StorePath, nixStoreDir)) {
		return nil, fmt.Errorf("narinfo is for %s, not %s", info.StorePath, hash)
	}

	if !upstream.Key.VerifyNarinfo(&info.NarInfo, info.Sigs) {
		return nil, fmt.Errorf("no valid signature by %s", upstream.Key.Name)
	}

	if maxNarSize > 0 && info.NarSize > maxNarSize {
		return nil, fmt.Errorf("NAR size %d exceeds max NAR size %d", info.NarSize, maxNarSize)
	}

	ext := narExtension[info.Compression]
	if ext == "" {
		return nil, fmt.Errorf("unsupported compression %q", info.Compression)
	}

	// Upstreams name NARs freely (cachix uses UUIDs); accept only a plain
	// relative path so the fetch stays on the upstream.
	if u, err := url.Parse(info.URL); err != nil || u.Scheme != "" || u.Host != "" ||
		strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path, "..") || u.RawQuery != "" {
		return nil, fmt.Errorf("unsupported NAR URL %q", info.URL)
	}

	info.NarKey = "nar/" + strings.TrimPrefix(info.NarHash, "sha256:") + ext
	if !IsValidUploadKey(info.NarKey, objectTypeNar) {
		return nil, fmt.Errorf("unsupported NarHash %q", info.NarHash)
	}

	return info, nil
}

// isStorePathBase reports whether base looks like "<32-char hash>-<name>".
func isStorePathBase(base string) bool {
	if len(base) < 34 || base[32] != '-' || strings.Contains(base, "/") {
		return false
	}

	for _, c := range base[:32] {
		if !strings.ContainsRune(api.NixBase32Alphabet, c) {
			return false
		}
	}

	return true
}

// narExtension maps the compressions niks3 can verify to their NAR suffix.
// shortcut: no xz; add a decoder if an upstream serves xz NARs.
var narExtension = map[string]string{ //nolint:gochecknoglobals // constant table
	compressionNone:  ".nar",
	compressionZstd:  ".nar.zst",
	compressionBzip2: ".nar.bz2",
}

// parseNarinfo parses narinfo text. Any field but Sig may appear once, so
// niks3 and Nix can't read a different value from the same text.
func parseNarinfo(text string) (*upstreamNarinfo, error) {
	info := &upstreamNarinfo{}
	seen := map[string]bool{}

	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		name, value, found := strings.Cut(line, ": ")
		if !found {
			return nil, fmt.Errorf("malformed narinfo line %q", line)
		}

		if name != "Sig" {
			if seen[name] {
				return nil, fmt.Errorf("duplicate narinfo field %q", name)
			}

			seen[name] = true
		}

		switch name {
		case "StorePath":
			info.StorePath = value
		case "URL":
			info.URL = value
		case "Compression":
			info.Compression = value
		case "NarHash":
			info.NarHash = value
		case "NarSize":
			size, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid NarSize %q: %w", value, err)
			}

			info.NarSize = size
		case "References":
			for ref := range strings.FieldsSeq(value) {
				if !isStorePathBase(ref) {
					return nil, fmt.Errorf("invalid reference %q", ref)
				}

				info.References = append(info.References, nixStoreDir+ref)
			}
		case "Deriver":
			info.Deriver = value
		case "CA":
			info.CA = value
		case "Sig":
			info.Sigs = append(info.Sigs, value)
		}
	}

	if info.Compression == "" {
		info.Compression = compressionBzip2 // Nix's default when the field is absent
	}

	return info, nil
}

// format renders the narinfo niks3 serves: the fields the signature covers
// plus the few Nix needs, with sigs appended. Fields niks3 doesn't check
// (FileHash, FileSize) are dropped rather than passed through unverified.
func (info *upstreamNarinfo) format(sigs []string) []byte {
	var b strings.Builder

	fmt.Fprintf(&b, "StorePath: %s\nURL: %s\nCompression: %s\nNarHash: %s\nNarSize: %d\n",
		info.StorePath, info.NarKey, info.Compression, info.NarHash, info.NarSize)

	refs := make([]string, len(info.References))
	for i, ref := range info.References {
		refs[i] = strings.TrimPrefix(ref, nixStoreDir)
	}

	fmt.Fprintf(&b, "References: %s\n", strings.Join(refs, " "))

	if info.Deriver != "" {
		fmt.Fprintf(&b, "Deriver: %s\n", info.Deriver)
	}

	if info.CA != "" {
		fmt.Fprintf(&b, "CA: %s\n", info.CA)
	}

	for _, sig := range sigs {
		fmt.Fprintf(&b, "Sig: %s\n", sig)
	}

	return []byte(b.String())
}

// storeUpstreamPath stores info's NAR and a re-signed narinfo, registered
// through a pending closure exactly as a push of that one path would be.
func (s *Service) storeUpstreamPath(ctx context.Context, upstream Upstream, info *upstreamNarinfo) error {
	hash := strings.TrimPrefix(info.StorePath, nixStoreDir)[:32]
	narinfoKey := hash + ".narinfo"

	refs := make([]string, 0, len(info.References)+1)
	for _, ref := range info.References {
		refs = append(refs, strings.TrimPrefix(ref, nixStoreDir)[:32]+".narinfo")
	}

	refs = append(refs, info.NarKey)

	pending, err := createPendingClosureInner(ctx, s.Pool, narinfoKey, nil, map[string]objectWithRefs{
		narinfoKey:  {Key: narinfoKey, Type: objectTypeNarinfo, Refs: refs},
		info.NarKey: {Key: info.NarKey, Type: objectTypeNar, Refs: []string{}, NarSize: &info.NarSize},
	}, s, true)
	if err != nil {
		return err
	}

	for _, obj := range pending.pendingObjects {
		if obj.Key == info.NarKey {
			if err := s.copyUpstreamNar(ctx, upstream, info); err != nil {
				return err
			}
		}
	}

	sigs := info.Sigs

	for _, signer := range s.SigningKeys {
		signed, err := signer.Sign(ctx, map[string]*signing.NarInfo{narinfoKey: &info.NarInfo})
		if err != nil {
			return fmt.Errorf("signing narinfo: %w", err)
		}

		sig, found := signed[narinfoKey]
		if !found {
			return errors.New("signing narinfo: signer returned no signature")
		}

		sigs = append(sigs, sig)
	}

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return fmt.Errorf("creating zstd encoder: %w", err)
	}

	compressed := encoder.EncodeAll(info.format(sigs), nil)

	if err := s.putObject(ctx, narinfoKey, bytes.NewReader(compressed), int64(len(compressed)),
		minio.PutObjectOptions{ContentType: "text/x-nix-narinfo", ContentEncoding: compressionZstd}); err != nil {
		return err
	}

	_, err = commitPendingClosure(ctx, s.Pool, pending.id)

	return err
}

// copyUpstreamNar streams info's NAR from upstream into the bucket while
// checking its NarHash and NarSize, and deletes it again if they don't match.
func (s *Service) copyUpstreamNar(ctx context.Context, upstream Upstream, info *upstreamNarinfo) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/"+info.URL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching NAR: %w", err)
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching NAR %s: status %d", info.URL, resp.StatusCode)
	}

	// Compressed data far beyond NarSize can't be a valid NAR for it.
	body := io.LimitReader(resp.Body, int64(info.NarSize)*2+1<<20) //nolint:gosec // NarSize <= MaxNarSize or a real file size

	pr, pw := io.Pipe()
	verified := make(chan error, 1)

	go func() {
		err := verifyNar(pr, info)
		if err != nil {
			cancel() // stop the upload early
		}

		_, _ = io.Copy(io.Discard, pr) // let the upload side finish writing

		verified <- err
	}()

	putErr := s.putObject(ctx, info.NarKey, io.TeeReader(body, pw), -1,
		minio.PutObjectOptions{ContentType: "application/x-nix-nar", PartSize: upstreamPartSize})
	pw.CloseWithError(putErr)

	if verifyErr := <-verified; verifyErr != nil {
		if putErr == nil {
			if err := s.MinioClient.RemoveObject(context.WithoutCancel(ctx), s.Bucket, info.NarKey, minio.RemoveObjectOptions{}); err != nil {
				slog.Error("Failed to remove unverified upstream NAR", "key", info.NarKey, "error", err)
			}
		}

		return fmt.Errorf("verifying NAR %s: %w", info.URL, verifyErr)
	}

	return putErr
}

// verifyNar decompresses r and checks it against info's NarHash and NarSize.
func verifyNar(r io.Reader, info *upstreamNarinfo) error {
	var nar io.Reader

	switch info.Compression {
	case compressionNone:
		nar = r
	case compressionBzip2:
		nar = bzip2.NewReader(r)
	case compressionZstd:
		decoder, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return fmt.Errorf("creating zstd decoder: %w", err)
		}
		defer decoder.Close()

		nar = decoder
	default:
		return fmt.Errorf("unsupported compression %q", info.Compression)
	}

	hasher := sha256.New()

	n, err := io.Copy(hasher, io.LimitReader(nar, int64(info.NarSize)+1)) //nolint:gosec // bounded by MaxNarSize
	if err != nil {
		return fmt.Errorf("decompressing: %w", err)
	}

	if uint64(n) != info.NarSize { //nolint:gosec // n >= 0
		return fmt.Errorf("NAR size mismatch: got %d, narinfo says %d", n, info.NarSize)
	}

	if got := "sha256:" + api.EncodeNixBase32(hasher.Sum(nil)); got != info.NarHash {
		return fmt.Errorf("NAR hash mismatch: got %s, narinfo says %s", got, info.NarHash)
	}

	return nil
}

// putObject uploads to the bucket under the S3 rate limiter.
func (s *Service) putObject(ctx context.Context, key string, r io.Reader, size int64, opts minio.PutObjectOptions) error {
	if err := s.S3RateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter: %w", err)
	}

	if _, err := s.MinioClient.PutObject(ctx, s.Bucket, key, r, size, opts); err != nil {
		if isRateLimitError(err) {
			s.S3RateLimiter.RecordThrottle()
		}

		return fmt.Errorf("uploading %s: %w", key, err)
	}

	s.S3RateLimiter.RecordSuccess()

	return nil
}

func closeBody(resp *http.Response) {
	if err := resp.Body.Close(); err != nil {
		slog.Debug("Failed to close response body", "error", err)
	}
}
