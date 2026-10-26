package importing

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Config builds browser links and the suggested calendar range from one coherent snapshot.
// It never requests the bank endpoint or handles a browser session.
func (s *Service) Config(ctx context.Context, user string) (Config, error) {
	state, zone, err := s.readSnapshot(ctx, user)
	if err != nil {
		return Config{}, err
	}
	if _, err := readUploadIndex(state.Uploads); err != nil {
		return Config{}, err
	}
	loaded, err := readLoaded(ctx, state.Operations, zone)
	if err != nil {
		return Config{}, err
	}
	now := s.now()
	if !readTime(now) {
		return Config{}, errAccounting
	}
	today := startDay(now, zone)
	day := min(today.Day(), time.Date(today.Year()-1, today.Month()+1, 0, 0, 0, 0, 0, zone).Day())
	from := time.Date(today.Year()-1, today.Month(), day, 0, 0, 0, 0, zone)
	if loaded != nil {
		from = startDay(loaded.Last, zone)
	}
	to := today.AddDate(0, 0, 1)
	if !readTime(from) || !readTime(to) || !from.Before(to) {
		return Config{}, errAccounting
	}
	query := url.Values{"appName": {"supreme"}, "appVersion": {"0.0.1"}, "origin": {"web,ib5,platform"}, "start": {strconv.FormatInt(from.UnixMilli(), 10)}, "end": {strconv.FormatInt(to.UnixMilli(), 10)}, "format": {"ofx"}}
	link := url.URL{Scheme: "https", Host: "www.tbank-online.com", Path: "/mybank/api/operations/timeline/public/legacy/v1/export_operations", RawQuery: query.Encode()}
	return Config{DownloadURL: link.String(), LoginURL: "https://www.tbank.ru/login/", TimeZone: zone.String(), From: from, To: to, Loaded: loaded}, nil
}

// History returns visible uploads, actual operation bounds and global covered-day gaps.
func (s *Service) History(ctx context.Context, user string) (History, error) {
	state, zone, err := s.readSnapshot(ctx, user)
	if err != nil {
		return History{}, err
	}
	loaded, err := readLoaded(ctx, state.Operations, zone)
	if err != nil {
		return History{}, err
	}
	index, err := readUploadIndex(state.Uploads)
	if err != nil {
		return History{}, err
	}
	counts, err := readUnreadCounts(ctx, state.Unread, index)
	if err != nil {
		return History{}, err
	}
	result := History{TimeZone: zone.String(), Loaded: loaded}
	var periods []Period
	for _, u := range state.Uploads {
		if err := ctx.Err(); err != nil {
			return History{}, err
		}
		if u.Status != "processing" && u.Status != "done" {
			continue
		}
		if !readTime(u.UploadedAt) || u.OperationCount < 0 {
			return History{}, errAccounting
		}
		u.UnreadCount, u.ResolvedUnreadCount, u.ResolvedByUploadedAt = 0, 0, nil
		if u.Status == "done" {
			c := counts[u.ID]
			u.UnreadCount, u.ResolvedUnreadCount, u.ResolvedByUploadedAt = c.open, c.read, c.latest
			u.Period, err = readUploadPeriod(ctx, u, state, zone)
			if err != nil {
				return History{}, err
			}
			if u.Period != nil {
				periods = append(periods, *u.Period)
			}
		} else {
			u.Period = nil
		}
		result.Uploads = append(result.Uploads, u)
	}
	merged, err := mergedDays(ctx, periods, zone)
	if err != nil {
		return History{}, err
	}
	result.Gaps = coverageGaps(merged)
	sort.Slice(result.Uploads, func(i, j int) bool {
		a, b := result.Uploads[i], result.Uploads[j]
		if !a.UploadedAt.Equal(b.UploadedAt) {
			return a.UploadedAt.After(b.UploadedAt)
		}
		return a.ID > b.ID
	})
	return result, nil
}

// Unread selects only unresolved rows of DONE uploads. Missing or foreign upload
// IDs return ErrNotFound (api.md 3.6); rows without dates never match a period.
func (s *Service) Unread(ctx context.Context, user string, scope Scope) ([]Unread, string, error) {
	if scope.UploadID == "" && scope.Period == nil {
		return nil, "", &InputError{"scope", "REQUIRED"}
	}
	if scope.UploadID != "" && scope.Period != nil {
		return nil, "", &InputError{"scope", "INVALID_FORMAT"}
	}
	if scope.Period != nil && (!readTime(scope.Period.From) || !readTime(scope.Period.To) || !scope.Period.From.Before(scope.Period.To)) {
		return nil, "", &InputError{"period", "OUT_OF_RANGE"}
	}
	state, zone, err := s.readSnapshot(ctx, user)
	if err != nil {
		return nil, "", err
	}
	index, err := readUploadIndex(state.Uploads)
	if err != nil {
		return nil, "", err
	}
	if scope.UploadID != "" {
		u, found := index[scope.UploadID]
		if !found || u.Status == "deleted" || u.Status == "staging" {
			return nil, "", ErrNotFound
		}
	}
	if _, err := readUnreadCounts(ctx, state.Unread, index); err != nil {
		return nil, "", err
	}
	var result []Unread
	for _, row := range state.Unread {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		u, found := index[row.UploadID]
		if !found || u.Status != "done" || row.ResolvedByUpload != "" {
			continue
		}
		if scope.UploadID != "" && row.UploadID != scope.UploadID {
			continue
		}
		if scope.Period != nil && (row.OccurredAt == nil || row.OccurredAt.Before(scope.Period.From) || !row.OccurredAt.Before(scope.Period.To)) {
			continue
		}
		row.FileName = u.FileName
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := index[result[i].UploadID], index[result[j].UploadID]
		if !a.UploadedAt.Equal(b.UploadedAt) {
			return a.UploadedAt.Before(b.UploadedAt)
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return result[i].Number < result[j].Number
	})
	return result, zone.String(), nil
}

func (s *Service) readSnapshot(ctx context.Context, user string) (Snapshot, *time.Location, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, nil, err
	}
	var state Snapshot
	err := s.tx.Read(ctx, func(readCtx context.Context) error {
		var err error
		state, err = s.repo.Snapshot(readCtx, user)
		return err
	})
	if err != nil {
		return Snapshot{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, nil, err
	}
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil || state.TimeZone == "" {
		zone, err = time.LoadLocation("Europe/Moscow")
	}
	if err != nil {
		return Snapshot{}, nil, errAccounting
	}
	return state, zone, nil
}

func readLoaded(ctx context.Context, operations []Operation, zone *time.Location) (*Loaded, error) {
	var result *Loaded
	for _, op := range operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !readTime(op.At) {
			return nil, errAccounting
		}
		at := op.At.In(zone)
		if result == nil {
			result = &Loaded{First: at, Last: at}
		}
		result.Count++
		result.First, result.Last = earlier(result.First, at), later(result.Last, at)
	}
	return result, nil
}

func readUploadIndex(uploads []Upload) (map[string]Upload, error) {
	result := make(map[string]Upload, len(uploads))
	for _, u := range uploads {
		if _, duplicate := result[u.ID]; duplicate || u.ID == "" {
			return nil, errAccounting
		}
		switch u.Status {
		case "processing", "done", "staging", "failed", "deleted":
		default:
			return nil, errAccounting
		}
		result[u.ID] = u
	}
	return result, nil
}

type unreadCounts struct {
	open, read int
	latest     *time.Time
}

type readSourceKey struct {
	accountID int64
	fitid     string
}

func readUnreadCounts(ctx context.Context, rows []Unread, uploads map[string]Upload) (map[string]unreadCounts, error) {
	result := make(map[string]unreadCounts)
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u, found := uploads[row.UploadID]
		if !found || u.Status != "done" {
			continue
		}
		if row.Number <= 0 || row.OccurredAt != nil && !readTime(*row.OccurredAt) {
			return nil, errAccounting
		}
		switch row.Reason {
		case "bad_amount", "bad_date", "no_date", "truncated":
		default:
			return nil, errAccounting
		}
		count := result[row.UploadID]
		if row.ResolvedByUpload == "" {
			if row.Resolution != "" || count.open == math.MaxInt32 {
				return nil, errAccounting
			}
			count.open++
		} else {
			resolver, ok := uploads[row.ResolvedByUpload]
			if !ok || resolver.Status != "done" || !readTime(resolver.UploadedAt) || resolver.ID == row.UploadID {
				return nil, errAccounting
			}
			switch row.Resolution {
			case "read":
				if count.read == math.MaxInt32 {
					return nil, errAccounting
				}
				count.read++
				if count.latest == nil || resolver.UploadedAt.After(*count.latest) {
					at := resolver.UploadedAt
					count.latest = &at
				}
			case "replaced":
			default:
				return nil, errAccounting
			}
		}
		result[row.UploadID] = count
	}
	return result, nil
}

func readUploadPeriod(ctx context.Context, u Upload, state Snapshot, zone *time.Location) (*Period, error) {
	var first, last *time.Time
	if u.Period != nil {
		first, last = &u.Period.From, &u.Period.To
	} else if u.Result != nil {
		first, last = u.Result.FirstOperationAt, u.Result.LastOperationAt
	} else {
		// Legacy rows without a result fall back to their current source membership.
		keys := make(map[readSourceKey]bool)
		for _, source := range state.Sources {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if source.UploadID == u.ID {
				if source.AccountID <= 0 || source.FITID == "" {
					return nil, errAccounting
				}
				keys[readSourceKey{source.AccountID, source.FITID}] = true
			}
		}
		for _, op := range state.Operations {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if keys[readSourceKey{op.AccountID, op.FITID}] {
				includeTime(&first, &last, op.At)
			}
		}
	}
	if (first == nil) != (last == nil) {
		return nil, errAccounting
	}
	if first == nil {
		return nil, nil
	}
	if !readTime(*first) || !readTime(*last) || first.After(*last) {
		return nil, errAccounting
	}
	return &Period{From: startDay(*first, zone), To: startDay(*last, zone)}, nil
}

func readTime(at time.Time) bool { return !at.IsZero() && at.Year() >= 1 && at.Year() <= 9999 }
