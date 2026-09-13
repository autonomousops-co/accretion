package queue

import "context"

// sharedClient is a Client whose Close is a no-op.
//
// Since #92 the log and metric collectors run in one process and hold the same
// queue client. Both call Close from their own Cleanup on shutdown, so whichever
// stopped first would close the connection out from under the other. Handing
// each of them a shared view makes Close the owning process's job, done once.
type sharedClient struct {
	Client
}

// Shared returns a view of c whose Close does nothing, for handing to several
// collectors that each believe they own their queue. The caller keeps c and
// closes it once.
func Shared(c Client) Client { return sharedClient{Client: c} }

// Close is deliberately a no-op; the owner of the underlying client closes it.
func (sharedClient) Close() error { return nil }

// CleanupWorkerQueue forwards to the underlying client when it supports it, so
// a collector still cleans up its own queue keys on shutdown even though it no
// longer owns the connection.
func (s sharedClient) CleanupWorkerQueue(ctx context.Context, workerID string) error {
	type cleaner interface {
		CleanupWorkerQueue(ctx context.Context, workerID string) error
	}
	if c, ok := s.Client.(cleaner); ok {
		return c.CleanupWorkerQueue(ctx, workerID)
	}
	return nil
}
