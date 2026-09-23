package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Mic92/niks3/server/pg"
	"github.com/minio/minio-go/v7"
)

const (
	DeletionBatchSize = 1000
)

// ObjectCleanupStats contains statistics about object cleanup operations.
type ObjectCleanupStats struct {
	MarkedCount  int
	DeletedCount int
	FailedCount  int
}

func flushBatch(ctx context.Context, keys []string, operation func(context.Context, []string) error) ([]string, error) {
	if len(keys) == 0 {
		return keys, nil
	}

	if err := operation(ctx, keys); err != nil {
		slog.Error("batch operation failed", "error", err)
		// Return keys unchanged to allow retry
		return keys, err
	}

	// Only clear keys on success
	return keys[:0], nil
}

func (s *Service) getObjectsForDeletion(ctx context.Context,
	objectCh chan<- minio.ObjectInfo,
	queryErr *error,
	stats *ObjectCleanupStats,
	gracePeriod int32,
	onProgress func(ObjectCleanupStats),
) {
	defer close(objectCh)

	queries := pg.New(s.Pool)

	// First, mark stale objects and get count
	marked, err := queries.MarkStaleObjects(ctx)
	if err != nil {
		*queryErr = fmt.Errorf("failed to mark stale objects: %w", err)
		slog.Error("failed to mark stale objects", "error", err)

		return
	}

	stats.MarkedCount = int(marked)

	if onProgress != nil {
		onProgress(*stats)
	}

	// Then, get objects ready for deletion (marked > gracePeriod ago).
	// Rows stay in this result set until removeS3Objects flushes them, which
	// may be long after they were handed out, so page by key: re-querying
	// from the start would hand the same keys out again, and on a backend
	// that reports nothing for an already-deleted key they would never flush.
	afterKey := ""

	for {
		objs, err := queries.GetObjectsReadyForDeletion(ctx, pg.GetObjectsReadyForDeletionParams{
			GracePeriodSeconds: gracePeriod,
			AfterKey:           afterKey,
			LimitCount:         DeletionBatchSize,
		})
		if err != nil {
			*queryErr = fmt.Errorf("failed to get objects ready for deletion: %w", err)
			slog.Error("failed to get objects ready for deletion", "error", err)

			break
		}

		if len(objs) == 0 {
			break
		}

		afterKey = objs[len(objs)-1]

		for _, obj := range objs {
			select {
			case objectCh <- minio.ObjectInfo{Key: obj}:
			case <-ctx.Done():
				*queryErr = ctx.Err()

				return
			}
		}
	}
}

// handleDeletedObject processes a successfully deleted object and flushes batch if needed.
func handleDeletedObject(ctx context.Context, objectName string, deletedKeys []string, queries *pg.Queries) ([]string, error) {
	deletedKeys = append(deletedKeys, objectName)

	if len(deletedKeys) >= DeletionBatchSize {
		var err error

		deletedKeys, err = flushBatch(ctx, deletedKeys, queries.DeleteObjects)

		return deletedKeys, err
	}

	return deletedKeys, nil
}

// handleFailedObject processes a failed deletion and flushes batch if needed.
func handleFailedObject(ctx context.Context, objectName string, resultErr error, failedKeys []string, queries *pg.Queries) ([]string, []error, error) {
	s3Errors := []error{fmt.Errorf("failed to remove object %q: %w", objectName, resultErr)}
	slog.Error("failed to remove object", "object", objectName, "error", resultErr)
	failedKeys = append(failedKeys, objectName)

	if len(failedKeys) >= DeletionBatchSize {
		var err error

		failedKeys, err = flushBatch(ctx, failedKeys, queries.MarkObjectsAsActive)

		return failedKeys, s3Errors, err
	}

	return failedKeys, s3Errors, nil
}

// unansweredKeys tracks the keys handed to RemoveObjectsWithResult that have
// not produced a result yet.
//
// minio-go does not answer for every key: on endpoints without multi-object
// delete (Google Cloud Storage) it deletes one key at a time and skips a key
// whose DELETE answers NoSuchKey without yielding anything. Such a key is
// already gone, and its row must still be dropped, or every later GC run
// hands it out again.
type unansweredKeys struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

func (u *unansweredKeys) add(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.keys[key] = struct{}{}
}

func (u *unansweredKeys) answered(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	delete(u.keys, key)
}

func (u *unansweredKeys) remaining() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	keys := make([]string, 0, len(u.keys))
	for key := range u.keys {
		keys = append(keys, key)
	}

	return keys
}

// trackUnanswered forwards objectCh, recording every key it passes on.
func trackUnanswered(ctx context.Context, objectCh <-chan minio.ObjectInfo, unanswered *unansweredKeys) <-chan minio.ObjectInfo {
	out := make(chan minio.ObjectInfo)

	go func() {
		defer close(out)

		for obj := range objectCh {
			unanswered.add(obj.Key)

			select {
			case out <- obj:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out
}

// isAbsent reports whether key is confirmed missing from the bucket. Any
// other outcome, including an error, is not a confirmation.
func (s *Service) isAbsent(ctx context.Context, key string) bool {
	_, err := s.MinioClient.StatObject(ctx, s.Bucket, key, minio.StatObjectOptions{})

	return err != nil && minio.ToErrorResponse(err).Code == minio.NoSuchKey
}

func (s *Service) removeS3Objects(ctx context.Context,
	objectCh <-chan minio.ObjectInfo,
	stats *ObjectCleanupStats,
	onProgress func(ObjectCleanupStats),
) ([]error, []error) {
	opts := minio.RemoveObjectsOptions{GovernanceBypass: false}
	failedKeys := make([]string, 0, DeletionBatchSize)
	deletedKeys := make([]string, 0, DeletionBatchSize)

	queries := pg.New(s.Pool)

	notifyProgress := func() {
		if onProgress != nil {
			onProgress(*stats)
		}
	}

	var s3Errors, batchErrors []error

	unanswered := &unansweredKeys{keys: map[string]struct{}{}}
	trackedCh := trackUnanswered(ctx, objectCh, unanswered)

	for result := range s.MinioClient.RemoveObjectsWithResult(ctx, s.Bucket, trackedCh, opts) {
		unanswered.answered(result.ObjectName)

		switch {
		case result.Err == nil:
			s.S3RateLimiter.RecordSuccess()
		case isRateLimitError(result.Err):
			// Track rate limit errors to enable adaptive rate limiting
			s.S3RateLimiter.RecordThrottle()
		}

		if result.Err != nil && minio.ToErrorResponse(result.Err).Code != minio.NoSuchKey {
			var (
				newS3Errors []error
				err         error
			)

			failedKeys, newS3Errors, err = handleFailedObject(ctx, result.ObjectName, result.Err, failedKeys, queries)

			s3Errors = append(s3Errors, newS3Errors...)
			if err != nil {
				batchErrors = append(batchErrors, err)
			}

			stats.FailedCount++
			notifyProgress()

			continue
		}

		// Deleted, or already absent from S3: either way the object is gone,
		// so drop the database row to keep S3 and the database consistent.
		var err error

		deletedKeys, err = handleDeletedObject(ctx, result.ObjectName, deletedKeys, queries)
		if err != nil {
			batchErrors = append(batchErrors, err)
		}

		stats.DeletedCount++
		notifyProgress()
	}

	// Keys minio-go said nothing about: drop the row of each one the bucket
	// confirms is gone. A key that cannot be confirmed keeps its row as it
	// is, and the next run hands it out again.
	for _, key := range unanswered.remaining() {
		if !s.isAbsent(ctx, key) {
			slog.Warn("object deletion produced no result and the object could not be confirmed absent", "object", key)

			continue
		}

		var err error

		deletedKeys, err = handleDeletedObject(ctx, key, deletedKeys, queries)
		if err != nil {
			batchErrors = append(batchErrors, err)
		}

		stats.DeletedCount++
		notifyProgress()
	}

	// Flush remaining batches
	if _, err := flushBatch(ctx, failedKeys, queries.MarkObjectsAsActive); err != nil {
		batchErrors = append(batchErrors, err)
	}

	if _, err := flushBatch(ctx, deletedKeys, queries.DeleteObjects); err != nil {
		batchErrors = append(batchErrors, err)
	}

	return s3Errors, batchErrors
}

// cleanupOrphanObjects marks unreachable objects and deletes them from S3.
// When onProgress is non-nil it is called after every individual
// mark/delete/fail so callers can expose live counters.
func (s *Service) cleanupOrphanObjects(ctx context.Context, gracePeriod int32, onProgress func(ObjectCleanupStats)) (*ObjectCleanupStats, error) {
	// limit channel size to 1000, as minio limits to 1000 in one request
	objectCh := make(chan minio.ObjectInfo, DeletionBatchSize)

	stats := &ObjectCleanupStats{}

	var queryErr error

	go s.getObjectsForDeletion(ctx, objectCh, &queryErr, stats, gracePeriod, onProgress)

	s3Errs, batchErrs := s.removeS3Objects(ctx, objectCh, stats, onProgress)

	if queryErr != nil {
		return stats, queryErr
	}

	// Prioritize batch errors (database operations) over S3 errors
	// as they're more critical for data integrity
	if len(batchErrs) > 0 {
		batchErr := errors.Join(batchErrs...)
		if len(s3Errs) > 0 {
			s3Err := errors.Join(s3Errs...)

			return stats, fmt.Errorf("%d batch operation failures: %w (also %d S3 failures: %w)",
				len(batchErrs), batchErr, len(s3Errs), s3Err)
		}

		return stats, fmt.Errorf("%d batch operation failures: %w", len(batchErrs), batchErr)
	}

	if len(s3Errs) > 0 {
		return stats, fmt.Errorf("%d S3 failures: %w", len(s3Errs), errors.Join(s3Errs...))
	}

	return stats, nil
}
