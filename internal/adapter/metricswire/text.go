package metricswire

import "fmt"

func transferNote(count int64) string {
	ending := "переводов"
	if count%100 < 11 || count%100 > 14 {
		switch count % 10 {
		case 1:
			ending = "перевод"
		case 2, 3, 4:
			ending = "перевода"
		}
	}
	return fmt.Sprintf("%d %s", count, ending)
}
