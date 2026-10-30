package objectstore

import (
	"context"
	"encoding/xml"
	"net/http"
)

const encryptionAlgorithm = "X-Amz-Server-Side-Encryption"
const encryptionKey = "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"

// checkEncryption is deliberately uncached: runtime cannot configure the bucket.
// Configuration and PUT are not atomic; operators must keep defaults immutable.
func (s *Store) checkEncryption(ctx context.Context) error {
	var config struct {
		XMLName xml.Name `xml:"ServerSideEncryptionConfiguration"`
		Rules   []struct {
			Defaults []struct {
				Algorithms []string `xml:"SSEAlgorithm"`
				Keys       []string `xml:"KMSMasterKeyID"`
			} `xml:"ApplyServerSideEncryptionByDefault"`
		} `xml:"Rule"`
	}
	if err := s.bucketConfig(ctx, "encryption", &config, false); err != nil {
		return err
	}
	if len(config.Rules) != 1 || len(config.Rules[0].Defaults) != 1 {
		return ErrConfig
	}
	defaults := config.Rules[0].Defaults[0]
	if len(defaults.Algorithms) != 1 || defaults.Algorithms[0] != "aws:kms" ||
		len(defaults.Keys) != 1 || defaults.Keys[0] != s.kmsKeyID {
		return ErrConfig
	}
	return nil
}

// Response validation precedes success/body exposure. A rejected PUT response
// still has an uncertain remote outcome; this never authorizes an automatic retry.
func (s *Store) encryptedResponse(h http.Header) bool {
	algorithms, keys := h.Values(encryptionAlgorithm), h.Values(encryptionKey)
	return len(algorithms) == 1 && algorithms[0] == "aws:kms" && len(keys) == 1 && keys[0] == s.kmsKeyID
}
