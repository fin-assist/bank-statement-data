package objectstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// CheckBucket rejects provider settings incompatible with permanent file erasure.
// Assembly calls it before accepting uploads. Operators must keep these settings
// immutable: an existing version or a disappearing fence defeats erasure.
func (s *Store) CheckBucket(ctx context.Context) error {
	var version struct {
		XMLName xml.Name `xml:"VersioningConfiguration"`
		Status  []string `xml:"Status"`
	}
	if err := s.bucketConfig(ctx, "versioning", &version, false); err != nil {
		return err
	}
	// S3 omits Status only when versioning has never been enabled. Suspended
	// buckets still retain earlier versions and therefore are unsafe too.
	if len(version.Status) != 0 {
		return ErrConfig
	}
	var lifecycle struct {
		XMLName xml.Name `xml:"LifecycleConfiguration"`
		Rules   []struct {
			Status     []string  `xml:"Status"`
			Expiration *struct{} `xml:"Expiration"`
		} `xml:"Rule"`
	}
	if err := s.bucketConfig(ctx, "lifecycle", &lifecycle, true); err != nil {
		return err
	}
	for _, rule := range lifecycle.Rules {
		if len(rule.Status) != 1 || rule.Status[0] != "Disabled" && rule.Status[0] != "Enabled" {
			return ErrConfig
		}
		if rule.Status[0] == "Enabled" && rule.Expiration != nil {
			return ErrConfig
		}
	}
	var lock struct {
		XMLName xml.Name `xml:"ObjectLockConfiguration"`
		Enabled []string `xml:"ObjectLockEnabled"`
	}
	if err := s.bucketConfig(ctx, "object-lock", &lock, true); err != nil {
		return err
	}
	if len(lock.Enabled) != 0 {
		return ErrConfig
	}
	return nil
}

func (s *Store) bucketConfig(ctx context.Context, kind string, out any, optional bool) error {
	req, err := s.request(ctx, http.MethodGet, "", url.Values{kind: {""}}, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if optional && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return ErrUnavailable
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	if d.Decode(out) != nil {
		return ErrUnavailable
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return ErrUnavailable
	}
	return nil
}
