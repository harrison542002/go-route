package adminapi

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/harrison542002/go-route/internal/ports"
)

// An audit cursor is base64 of the (ts, id) ordering key. It is opaque by
// contract, not secret: the encoding stops callers depending on its shape, and
// a forged one buys nothing a plain `since` would not.

// cursorVersion is the leading field, so a future encoding can be told from
// this one rather than guessed at from its length.
const cursorVersion = "1"

// encodeCursor renders a position as the string a caller passes back. The
// timestamp goes in as Unix nanoseconds so it round-trips exactly; a formatted
// time would lose the microseconds the ordering depends on.
func encodeCursor(c ports.AuditCursor) string {
	raw := strings.Join([]string{cursorVersion, strconv.FormatInt(c.At.UnixNano(), 10), c.ID.String()}, ".")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reads a cursor back, refusing anything it did not write.
// Garbage must be an error, not a silent restart from the newest event: a
// paging client handed page one instead of page five loops forever.
func decodeCursor(s string) (ports.AuditCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ports.AuditCursor{}, fmt.Errorf("cursor is not valid base64")
	}

	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return ports.AuditCursor{}, fmt.Errorf("cursor is malformed")
	}

	nanos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ports.AuditCursor{}, fmt.Errorf("cursor carries no timestamp")
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return ports.AuditCursor{}, fmt.Errorf("cursor carries no id")
	}

	return ports.AuditCursor{At: time.Unix(0, nanos).UTC(), ID: id}, nil
}
