package domains

type TokenUsage struct {
	Input      int
	Output     int
	CacheWrite int
	CacheRead  int
	Reasoning  int
}
