package domain

const (
	// MetricStatusReady identifies a metric ready for display.
	MetricStatusReady = "ready"
	// MetricStatusTentative identifies a usable preliminary metric.
	MetricStatusTentative = "tentative"
	// MetricStatusLocked identifies a metric whose calculation requirements are not met.
	MetricStatusLocked = "locked"
	// MetricStatusNone identifies a metric without a result to display.
	MetricStatusNone = "none"
)
