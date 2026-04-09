package analytics

// Backend is the interface for analytics storage.
// The default in-memory Collector satisfies this interface.
// Production deployments can implement this for ClickHouse, BigQuery, etc.
type Backend interface {
	// Ingest processes a batch of log entries.
	Ingest(entries []LogEntry) error

	// Query returns stats for a domain, or all domains if hostname is empty.
	Query(hostname string) ([]*DomainStats, error)

	// Close gracefully shuts down the backend, flushing any pending data.
	Close() error
}
