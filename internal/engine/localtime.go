package engine

// Host time zone plumbing for Date.
//
// Date stores a UTC time value; "local time" is that value shifted by the host
// zone's offset at that instant. Local used to be defined as UTC here, which
// kept conformance runs from depending on the machine's zone but diverged from
// V8: on a UTC+03 host, getHours() was three hours out and the date rolled over
// three hours late, so a flow reading "today" between midnight and 03:00 got
// yesterday. Conformance is now pinned with TZ=UTC instead (see
// conformance/README), which restores the property without changing semantics.

import (
	"os"
	"strings"
	"sync"
	"time"
)

// localLoc is the zone Date resolves local time against. V8 reads the host zone
// once when the isolate starts and caches it; time.Local has the same lifetime
// and honours $TZ the same way, so the two agree without extra plumbing.
func localLoc() *time.Location { return time.Local }

// msToLocal converts a time value to the host zone's wall clock, msToTime to
// UTC's. Every local getter goes through the first and every UTC getter through
// the second; nothing else may assume the two agree.
func msToLocal(ms float64) time.Time {
	return time.UnixMilli(int64(ms)).In(localLoc())
}

// localOffsetMs is the host zone's offset from UTC, in milliseconds, at the
// instant denoted by the (UTC) time value ms.
func localOffsetMs(ms float64) float64 {
	_, off := msToLocal(ms).Zone()
	return float64(off) * msPerSecond
}

// utcFromLocalMs is the spec's UTC(t): it maps a local wall-clock time value to
// the UTC time value denoting the same instant.
//
// The offset has to be looked up twice. The first lookup treats t as though it
// were already UTC, which is wrong by exactly the offset, and the second lookup
// is taken at the instant that first guess points to. One pass is not enough
// because the offset in effect is a property of the instant being solved for,
// not of the wall clock reading it.
//
// Around a DST transition the mapping is not one-to-one: a wall clock in the
// spring-forward gap denotes no instant and one in the autumn overlap denotes
// two. Go resolves both the way V8 does, by taking the offset in effect after
// the second lookup, so the gap maps forward and the overlap picks the earlier
// instant.
func utcFromLocalMs(ms float64) float64 {
	// Beyond the Date range the value is destined for TimeClip's NaN, and the
	// int64 conversion inside the offset lookup would overflow first.
	if ms != ms || ms > maxTimeValue+msPerDay || ms < -maxTimeValue-msPerDay {
		return ms
	}
	guess := ms - localOffsetMs(ms)
	return ms - localOffsetMs(guess)
}

// localZoneID reports the host zone's IANA name, which is the key
// zoneDisplayNames is built on.
//
// time.Local reports "Local" when Go loaded /etc/localtime as a bare file
// rather than resolving a name, so fall back to $TZ and then to the symlink
// target, which is how ICU identifies the zone as well. A host whose zone
// cannot be named this way still formats correctly, just with the GMT+HH:MM
// display name rather than the long one.
var localZoneID = sync.OnceValue(func() string {
	if n := localLoc().String(); n != "" && n != "Local" {
		return n
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		return tz
	}
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.LastIndex(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
	}
	return localLoc().String()
})

// zoneDisplayName is the long zone name Date.prototype.toString() prints in
// parentheses, e.g. "Eastern Standard Time" or "GMT+03:00". ICU picks the
// daylight or standard name by whether DST is in effect at that instant, not by
// whether the zone observes it at all, so a single zone has both.
func zoneDisplayName(t time.Time) string {
	if pair, ok := zoneDisplayNames[localZoneID()]; ok {
		if t.IsDST() {
			return pair[1]
		}
		return pair[0]
	}
	return gmtOffsetName(t)
}

// gmtOffsetName is the fallback display name for a zone CLDR carries no name
// for, in ICU's localised-GMT format.
func gmtOffsetName(t time.Time) string {
	_, off := t.Zone()
	if off == 0 {
		return "GMT"
	}
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	h, m := off/3600, (off%3600)/60
	return "GMT" + sign + twoDigits(h) + ":" + twoDigits(m)
}

func twoDigits(n int) string {
	if n < 10 {
		return string([]byte{'0', byte('0' + n)})
	}
	return string([]byte{byte('0' + n/10%10), byte('0' + n%10)})
}
