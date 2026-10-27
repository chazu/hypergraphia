package entry

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	mintMu  sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// Mint returns a new ULID that sorts after every ULID in floor (the latest
// entry already seen, plus any targets the new entry references) and is at
// least now. Minting causally keeps a retract or supersede from sorting before
// its target when this machine's clock is behind the writer of that target.
func Mint(now time.Time, floor ...string) (ulid.ULID, error) {
	ms := ulid.Timestamp(now)
	for _, f := range floor {
		id, err := ulid.ParseStrict(f)
		if err != nil {
			continue
		}
		if t := id.Time() + 1; t > ms {
			ms = t
		}
	}
	mintMu.Lock()
	defer mintMu.Unlock()
	return ulid.New(ms, entropy)
}
