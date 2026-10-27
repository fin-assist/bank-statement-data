package domain

import "time"

// DailyRecalculationOffset schedules daily recalculation at 00:05 in the user's
// calendar, as required by contracts/docs/grpc-api.md 7.3. Calendar helpers retain
// the first real day start when a zone skips midnight or an entire date.
const DailyRecalculationOffset = 5 * time.Minute
