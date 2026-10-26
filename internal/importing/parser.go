package importing

import (
	"context"
	"io"
)

// Parser separates synchronous source admission from background transaction parsing.
// Implementations preserve original physical row positions and use +03:00 for
// source timestamps without an explicit offset.
type Parser interface {
	Validate(io.Reader) error
	Parse(context.Context, io.Reader) (Document, error)
}

// Refusal is a safe synchronous source classification, independent of transport or UI text.
type Refusal string

const (
	// ErrCSVNotAccepted rejects CSV content independently of its filename.
	ErrCSVNotAccepted Refusal = "csv_not_accepted"
	// ErrWrongFormat rejects content that is not an accepted OFX document.
	ErrWrongFormat Refusal = "wrong_format"
	// ErrWrongBank rejects an OFX document belonging to another bank.
	ErrWrongBank Refusal = "wrong_bank"
)

func (r Refusal) Error() string { return string(r) }
