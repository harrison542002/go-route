package ports

// ProviderRequest is a resolved dispatch instruction: which upstream
// model, which body, and whether the client wanted usage reporting.
type ProviderRequest struct {
	Model      string
	Body       []byte
	Stream     bool
	WantsUsage bool
}
