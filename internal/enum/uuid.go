package enum

import (
	"crypto/rand"
	"fmt"
)

// uuidv4 returns a random RFC 4122 version-4 UUID string, generated from
// crypto/rand (replaces github.com/google/uuid, the only use here being the
// buvid3 device cookie). Panics only when the OS entropy source fails —
// the same behavior as google/uuid.New, which cannot happen on supported
// platforms in practice.
func uuidv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("uuid: crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant (10xx)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
