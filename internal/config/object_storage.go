package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// ObjectStorageConfig identifies the private, unversioned statement bucket.
// IAMTokenFile is an externally rotated credential path, never a token value.
type ObjectStorageConfig struct {
	Endpoint     string
	Bucket       string
	KMSKeyID     string
	IAMTokenFile string
}

func (r *reader) objectStorage() ObjectStorageConfig {
	return ObjectStorageConfig{Endpoint: r.str("OBJECT_STORAGE_ENDPOINT"), Bucket: r.str("OBJECT_STORAGE_BUCKET"), KMSKeyID: r.str("OBJECT_STORAGE_KMS_KEY_ID"), IAMTokenFile: r.str("OBJECT_STORAGE_IAM_TOKEN_FILE")}
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func (c Config) validateObjectStorage() []error {
	var errs []error
	s := c.ObjectStorage
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		errs = append(errs, errors.New("OBJECT_STORAGE_ENDPOINT requires HTTPS without credentials, path, query or fragment"))
	}
	if !bucketName.MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") {
		errs = append(errs, errors.New("OBJECT_STORAGE_BUCKET requires a valid private bucket name"))
	}
	if s.KMSKeyID == "" || len(s.KMSKeyID) > 16384 {
		errs = append(errs, errors.New("OBJECT_STORAGE_KMS_KEY_ID is required"))
	} else {
		for _, r := range s.KMSKeyID {
			if r < 33 || r > 126 {
				errs = append(errs, errors.New("OBJECT_STORAGE_KMS_KEY_ID contains invalid characters"))
				break
			}
		}
	}
	if !filepath.IsAbs(s.IAMTokenFile) || strings.ContainsRune(s.IAMTokenFile, 0) {
		errs = append(errs, errors.New("OBJECT_STORAGE_IAM_TOKEN_FILE requires an absolute credential file path"))
	}
	return errs
}
