// Package importing owns durable statement admission, processing and removal.
package importing

import "time"

// Document is parsed source data, before database identity, deduplication or accounting rules.
// It deliberately retains no raw unread-line contents.
type Document struct {
	Accounts []Account
	Lines    []Line
	Periods  []Period
}

// Account identifies a source account. Key is the opaque ACCTID stored as ext_id;
// it must never be returned by public APIs or written to logs or error messages.
type Account struct {
	Key       string
	Type      string
	Mask      *string // Last four characters only; no presentation prefix. Nil for OB$ accounts.
	OtherBank bool
}

// Period contains a complete pair of source DTSTART/DTEND values for one account.
// The usecase projects their inclusive calendar days in the user's profile zone.
type Period struct {
	AccountKey string
	From       time.Time
	To         time.Time
	// RawFrom/RawTo retain source instants before projection into profile days.
	RawFrom, RawTo *time.Time
	// Source distinguishes explicit tag coverage from operation fallback.
	Source string
}

// Line is one source transaction or its unread metadata. An empty Reason means
// the transaction is readable; otherwise it is no_date, bad_date, bad_amount or truncated.
type Line struct {
	Number       int // Original one-based physical line containing the opening STMTTRN tag.
	AccountKey   string
	FITID        string
	OccurredAt   *time.Time
	AmountMinor  int64
	Currency     string // CURSYM, or RUB when the source omits it.
	Direction    string // expense or income, as decided from TRNAMT and TRNTYPE.
	Description  string
	BankCategory string
	Reason       string
}
