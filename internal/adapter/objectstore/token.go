package objectstore

import (
	"context"
	"io"
	"os"
	"strings"
)

// FileToken reloads an externally rotated IAM token. It never returns path or token diagnostics.
// The application supplies a credential file path; this adapter reads no environment variables.
func FileToken(path string) TokenProvider {
	return func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		f, err := os.Open(path) // #nosec G304 -- credential path is trusted application configuration, never RPC input.
		if err != nil {
			return "", ErrUnavailable
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
			return "", ErrUnavailable
		}
		data, err := io.ReadAll(io.LimitReader(f, 16385))
		if err != nil || len(data) > 16384 {
			return "", ErrUnavailable
		}
		token := strings.TrimSpace(string(data))
		if !headerValue(token) {
			return "", ErrUnavailable
		}
		return token, nil
	}
}
