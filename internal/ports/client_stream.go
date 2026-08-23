package ports

// ClientStream is the downstream connection to the caller.
type ClientStream interface {

	// Commit writes response headers and status.
	// Called at most once, only after an upstream has yielded its first event
	Commit() error

	// Send writes one event and flushes it
	Send(ev StreamEvent) error

	SendError(err error) error
}
