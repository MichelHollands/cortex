package chunk

import "context"

// StorageClient is a client for the persistent storage for Cortex. (e.g. DynamoDB + S3).
type StorageClient interface {
	// For the write path.
	NewWriteBatch() WriteBatch
	BatchWrite(context.Context, WriteBatch) error

	// For the read path.
	QueryPages(ctx context.Context, queries []IndexQuery, callback func(IndexQuery, ReadBatch) (shouldContinue bool)) error

	// For storing and retrieving chunks.
	PutChunks(ctx context.Context, chunks []Chunk) error
	GetChunks(ctx context.Context, chunks []Chunk) ([]Chunk, error)

	NewStreamer() Streamer
}

// WriteBatch represents a batch of writes.
type WriteBatch interface {
	Add(tableName, hashValue string, rangeValue []byte, value []byte)
}

// ReadBatch represents the results of a QueryPages.
type ReadBatch interface {
	Iterator() ReadBatchIterator
}

// ReadBatchIterator is an iterator over a ReadBatch.
type ReadBatchIterator interface {
	Next() bool
	RangeValue() []byte
	Value() []byte
}

// Streamer represents the configuration for streaming chunks
type Streamer interface {
	Add(string, string, int, int)
	Stream(context.Context, chan []Chunk) error
	Size(context.Context) (int, error)
}
