package importing

import (
	"context"
	"sort"
	"time"
)

type sourceIdentity struct {
	account int64
	fitid   string
}
type closureFacts struct {
	upload   Upload
	accounts map[int64]bool
	read     map[sourceIdentity]bool
	unread   map[sourceIdentity]bool
	dates    map[int64][]time.Time
	noDate   map[int64]bool
}

// ResolveUnread recomputes FIN-79 closures from all remaining completed uploads.
// Original unread facts remain available even if a later upload has closed them.
// The repository saves these projections under its per-user transaction lock.
func ResolveUnread(ctx context.Context, uploads []Upload, unread []Unread, sources []Source, zone *time.Location) ([]Unread, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if zone == nil {
		return nil, errAccounting
	}
	byID := make(map[string]*closureFacts)
	var ordered []*closureFacts
	for _, upload := range uploads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if upload.Status != "done" {
			continue
		}
		if upload.ID == "" || upload.UploadedAt.IsZero() || byID[upload.ID] != nil || upload.Period != nil && (upload.Period.From.IsZero() || upload.Period.To.IsZero() || upload.Period.From.After(upload.Period.To)) {
			return nil, errAccounting
		}
		f := &closureFacts{upload: upload, accounts: make(map[int64]bool), read: make(map[sourceIdentity]bool), unread: make(map[sourceIdentity]bool), dates: make(map[int64][]time.Time), noDate: make(map[int64]bool)}
		for _, id := range upload.AccountIDs {
			f.accounts[id] = true
		}
		byID[upload.ID] = f
		ordered = append(ordered, f)
	}
	sort.Slice(ordered, func(i, j int) bool { return uploadBefore(ordered[i].upload, ordered[j].upload) })
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if f := byID[source.UploadID]; f != nil {
			f.read[sourceIdentity{source.AccountID, source.FITID}] = true
		}
	}
	for _, line := range unread {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := byID[line.UploadID]
		if f == nil || line.AccountID == nil {
			continue
		}
		account := *line.AccountID
		if line.FITID != "" {
			f.unread[sourceIdentity{account, line.FITID}] = true
		}
		if line.OccurredAt == nil {
			f.noDate[account] = true
		} else {
			f.dates[account] = append(f.dates[account], startDay(*line.OccurredAt, zone))
		}
	}
	for _, f := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, dates := range f.dates {
			sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
		}
	}
	result := append([]Unread(nil), unread...)
	for i := range result {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := &result[i]
		line.ResolvedByUpload, line.Resolution = "", ""
		old := byID[line.UploadID]
		if old == nil || line.AccountID == nil {
			continue
		}
		for _, candidate := range ordered {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !uploadBefore(old.upload, candidate.upload) || !candidate.accounts[*line.AccountID] {
				continue
			}
			resolution := resolveLine(*line, old.upload, candidate, zone)
			if resolution != "" {
				line.ResolvedByUpload, line.Resolution = candidate.upload.ID, resolution
				break
			}
		}
	}
	return result, nil
}

func uploadBefore(a, b Upload) bool {
	return a.UploadedAt.Before(b.UploadedAt) || a.UploadedAt.Equal(b.UploadedAt) && a.ID < b.ID
}

func resolveLine(line Unread, old Upload, candidate *closureFacts, zone *time.Location) string {
	account := *line.AccountID
	if line.FITID != "" {
		identity := sourceIdentity{account, line.FITID}
		if candidate.read[identity] {
			return "read"
		}
		if candidate.unread[identity] {
			return "replaced"
		}
		return ""
	}
	period := candidate.upload.Period
	if period == nil {
		return ""
	}
	from, to := startDay(period.From, zone), startDay(period.To, zone)
	if line.OccurredAt != nil {
		day := startDay(*line.OccurredAt, zone)
		if day.Before(from) || day.After(to) {
			return ""
		}
		if hasDatedUnread(candidate.dates[account], day, day) {
			return "replaced"
		}
		return "read"
	}
	if old.Period == nil {
		return ""
	}
	oldFrom, oldTo := startDay(old.Period.From, zone), startDay(old.Period.To, zone)
	if oldFrom.Before(from) || oldTo.After(to) {
		return ""
	}
	if candidate.noDate[account] || hasDatedUnread(candidate.dates[account], oldFrom, oldTo) {
		return "replaced"
	}
	return "read"
}

func hasDatedUnread(dates []time.Time, from, to time.Time) bool {
	i := sort.Search(len(dates), func(i int) bool { return !dates[i].Before(from) })
	return i < len(dates) && !dates[i].After(to)
}
