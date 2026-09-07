package claudeprint

import (
	"crypto/rand"
	"fmt"
)

// newSessionID returns a random RFC 4122 version-4 UUID, used as the
// --session-id a chat's first turn establishes and every later turn resumes
// via --resume. The module has no existing UUID dependency, so this is a
// minimal local generator rather than pulling one in for a single call site.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on a supported platform does not fail in
		// practice; panicking here would be worse than a locally
		// distinctive placeholder that still lets the process continue.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
