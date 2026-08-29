package calendar

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

// crockford is the Crockford base32 alphabet used by ULIDs (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID returns a prefixed, lexicographically-sortable id: prefix + 26-char
// ULID. The first 10 chars encode a 48-bit millisecond timestamp, the last 16
// encode 80 bits of randomness, so ids sort by creation time. Stdlib only.
// (Local copy of the botnet id scheme — no cross-package import.)
func newID(prefix string) string {
	var buf [16]byte
	ms := uint64(time.Now().UnixMilli())
	buf[0] = byte(ms >> 40)
	buf[1] = byte(ms >> 32)
	buf[2] = byte(ms >> 24)
	buf[3] = byte(ms >> 16)
	buf[4] = byte(ms >> 8)
	buf[5] = byte(ms)
	if _, err := rand.Read(buf[6:]); err != nil {
		// crypto/rand failure is unrecoverable; fall back to time entropy.
		binary.BigEndian.PutUint64(buf[8:], uint64(time.Now().UnixNano()))
	}

	// Encode 128 bits as 26 Crockford base32 chars (ULID canonical form).
	var out [26]byte
	out[0] = crockford[(buf[0]&224)>>5]
	out[1] = crockford[buf[0]&31]
	out[2] = crockford[(buf[1]&248)>>3]
	out[3] = crockford[((buf[1]&7)<<2)|((buf[2]&192)>>6)]
	out[4] = crockford[(buf[2]&62)>>1]
	out[5] = crockford[((buf[2]&1)<<4)|((buf[3]&240)>>4)]
	out[6] = crockford[((buf[3]&15)<<1)|((buf[4]&128)>>7)]
	out[7] = crockford[(buf[4]&124)>>2]
	out[8] = crockford[((buf[4]&3)<<3)|((buf[5]&224)>>5)]
	out[9] = crockford[buf[5]&31]
	out[10] = crockford[(buf[6]&248)>>3]
	out[11] = crockford[((buf[6]&7)<<2)|((buf[7]&192)>>6)]
	out[12] = crockford[(buf[7]&62)>>1]
	out[13] = crockford[((buf[7]&1)<<4)|((buf[8]&240)>>4)]
	out[14] = crockford[((buf[8]&15)<<1)|((buf[9]&128)>>7)]
	out[15] = crockford[(buf[9]&124)>>2]
	out[16] = crockford[((buf[9]&3)<<3)|((buf[10]&224)>>5)]
	out[17] = crockford[buf[10]&31]
	out[18] = crockford[(buf[11]&248)>>3]
	out[19] = crockford[((buf[11]&7)<<2)|((buf[12]&192)>>6)]
	out[20] = crockford[(buf[12]&62)>>1]
	out[21] = crockford[((buf[12]&1)<<4)|((buf[13]&240)>>4)]
	out[22] = crockford[((buf[13]&15)<<1)|((buf[14]&128)>>7)]
	out[23] = crockford[(buf[14]&124)>>2]
	out[24] = crockford[((buf[14]&3)<<3)|((buf[15]&224)>>5)]
	out[25] = crockford[buf[15]&31]

	return prefix + string(out[:])
}

// validEventID reports whether a client-supplied event id has exactly the
// shape newID emits: "evt_" + 26 uppercase Crockford base32 characters. It is
// client input reaching a primary key, so the check is strict — nothing
// looser than what the server itself would mint.
func validEventID(id EventID) bool {
	const prefix = "evt_"
	if len(id) != len(prefix)+26 || !strings.HasPrefix(string(id), prefix) {
		return false
	}
	for _, c := range id[len(prefix):] {
		if !strings.ContainsRune(crockford, c) {
			return false
		}
	}
	return true
}
