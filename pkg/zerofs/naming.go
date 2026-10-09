package zerofs

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxK8sNameLen is the longest name the manager generates.  Service names are
// limited to 63 characters and label values to the same, so every object is
// kept below that.
const maxK8sNameLen = 63

// k8sName maps an opaque CSI volume ID - which the driver has to accept as an
// arbitrary string - onto a lowercase RFC 1123 name that is valid both as a
// Kubernetes object name and as a label value.  maxLen is the number of
// characters the caller has left after its own prefix.
//
// Volume IDs produced by csi-sanity contain upper case characters and may
// exceed the Kubernetes length limits, so the ID is rewritten when needed.  A
// short hash of the original ID is appended whenever it was rewritten, which
// keeps the mapping collision free.  IDs that are already valid (the
// "pvc-<uid>" form the external-provisioner uses) are returned unchanged.
func k8sName(volumeID string, maxLen int) string {
	safe := sanitizeRFC1123(volumeID)
	if safe == volumeID && len(safe) <= maxLen {
		return safe
	}

	hash := hashVolumeID(volumeID)
	keep := maxLen - len(hash) - 1
	if keep < 1 {
		return hash
	}

	if len(safe) > keep {
		safe = safe[:keep]
	}
	safe = strings.Trim(safe, "-.")
	if safe == "" {
		return hash
	}
	return safe + "-" + hash
}

// sanitizeRFC1123 lowercases the input and replaces every character that is not
// allowed in a lowercase RFC 1123 subdomain with a dash.
func sanitizeRFC1123(in string) string {
	var b strings.Builder
	b.Grow(len(in))

	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
			b.WriteByte(c)
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c - 'A' + 'a')
		default:
			b.WriteByte('-')
		}
	}

	return strings.Trim(b.String(), "-.")
}

func hashVolumeID(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return hex.EncodeToString(sum[:])[:8]
}
