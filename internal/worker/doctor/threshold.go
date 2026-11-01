package doctor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Autarquico/codefabric/internal/domain"
)

// DefaultMinFree is the free disk below which a Worker fails (D10).
const DefaultMinFree = "20%"

// hysteresisPoints is how far above the threshold the free space has to climb
// before a failing disk is ok again, so a disk that hovers does not flap.
const hysteresisPoints = 2

// Threshold is the least free space a Worker accepts: a share of the disk or
// an amount of bytes, never both.
type Threshold struct {
	Percent float64
	Bytes   uint64
}

var sizeUnits = []struct {
	suffix string
	factor uint64
}{
	{"TIB", 1 << 40},
	{"GIB", 1 << 30},
	{"MIB", 1 << 20},
	{"TB", 1 << 40},
	{"GB", 1 << 30},
	{"MB", 1 << 20},
}

// ParseThreshold reads "20%" or a size such as "50GiB" (units are powers of 1024).
func ParseThreshold(text string) (Threshold, error) {
	bad := domain.NewError(domain.CodeInvalidRequest,
		fmt.Sprintf("--disk-min-free must be a percentage such as 20%% or a size such as 50GiB, got %q", text), nil)
	text = strings.TrimSpace(text)
	if digits, ok := strings.CutSuffix(text, "%"); ok {
		p, err := strconv.ParseFloat(digits, 64)
		if err != nil || p <= 0 || p >= 100 {
			return Threshold{}, bad
		}
		return Threshold{Percent: p}, nil
	}
	upper := strings.ToUpper(text)
	for _, u := range sizeUnits {
		digits, ok := strings.CutSuffix(upper, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(digits, 10, 64)
		if err != nil || n == 0 || n > (1<<62)/u.factor {
			return Threshold{}, bad
		}
		return Threshold{Bytes: n * u.factor}, nil
	}
	return Threshold{}, bad
}

// String is the threshold as ParseThreshold reads it.
func (t Threshold) String() string {
	if t.Bytes > 0 {
		return humanBytes(t.Bytes)
	}
	return strconv.FormatFloat(t.Percent, 'f', -1, 64) + "%"
}

// floor is the free bytes below which a disk of total bytes fails.
func (t Threshold) floor(total uint64) uint64 {
	if t.Bytes > 0 {
		return t.Bytes
	}
	return uint64(float64(total) * t.Percent / 100)
}

// Failing tells whether free bytes of a disk of total bytes are too few. A disk
// that was failing stays failing until it is hysteresisPoints of the disk above
// the threshold.
func (t Threshold) Failing(free, total uint64, wasFailing bool) bool {
	limit := t.floor(total)
	if wasFailing {
		limit += total / 100 * hysteresisPoints
	}
	return free < limit
}

func humanBytes(n uint64) string {
	for _, u := range []struct {
		name   string
		factor uint64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}} {
		if n >= u.factor {
			return strconv.FormatFloat(float64(n)/float64(u.factor), 'f', 1, 64) + u.name
		}
	}
	return strconv.FormatUint(n, 10) + "B"
}
