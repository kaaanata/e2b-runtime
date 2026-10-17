package blockvol

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrResourceExhausted is the sentinel every CapacityError matches with
// errors.Is; the gRPC layer maps it to codes.ResourceExhausted.
var ErrResourceExhausted = errors.New("blockvol: insufficient capacity")

// CapacityError reports which Btrfs pool (data or metadata) is below its
// watermark for the requested operation.
type CapacityError struct {
	Pool      string // "data" or "metadata"
	Free      uint64
	Required  uint64
	Watermark uint64
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("blockvol: insufficient %s capacity: free %d, required %d plus watermark %d", e.Pool, e.Free, e.Required, e.Watermark)
}

func (e *CapacityError) Is(target error) bool { return target == ErrResourceExhausted }

// Watermarks are the minimum free bytes that must remain after an operation.
// Metadata exhaustion is a Btrfs-specific failure that makes the filesystem
// read-only, so it gets its own reserve.
type Watermarks struct {
	MinDataFree     uint64
	MinMetadataFree uint64
}

// DefaultWatermarks keeps 2 GiB of data and 512 MiB of metadata headroom.
var DefaultWatermarks = Watermarks{
	MinDataFree:     2 << 30,
	MinMetadataFree: 512 << 20,
}

// Usage is the parsed `btrfs filesystem usage -b` summary.
type Usage struct {
	DeviceSize        uint64
	DeviceUnallocated uint64
	FreeEstimated     uint64
	GlobalReserve     uint64
	DataRatio         float64
	MetadataRatio     float64
	DataSize          uint64
	DataUsed          uint64
	MetadataSize      uint64
	MetadataUsed      uint64
	SystemSize        uint64
	SystemUsed        uint64
}

// DataFree is the space a new file can still consume: slack inside existing
// data chunks plus unallocated device space that Btrfs would turn into data
// chunks, scaled by the data profile ratio.
func (u Usage) DataFree() uint64 {
	ratio := u.DataRatio
	if ratio <= 0 {
		ratio = 1
	}

	return u.DataSize - u.DataUsed + uint64(float64(u.DeviceUnallocated)/ratio)
}

// MetadataFree is computed like DataFree with the metadata profile ratio.
func (u Usage) MetadataFree() uint64 {
	ratio := u.MetadataRatio
	if ratio <= 0 {
		ratio = 1
	}

	return u.MetadataSize - u.MetadataUsed + uint64(float64(u.DeviceUnallocated)/ratio)
}

var (
	chunkLine   = regexp.MustCompile(`^(Data|Metadata|System),\S+:\s+Size:(\d+),\s+Used:(\d+)`)
	overallLine = regexp.MustCompile(`^\s*([A-Za-z ,()]+?):\s+([\d.]+)`)
)

// ParseUsage parses the output of `btrfs filesystem usage -b <path>`. The -b
// flag makes every size a plain byte count; without root the per-device
// section is missing but the overall and chunk lines parsed here remain.
func ParseUsage(out []byte) (Usage, error) {
	var u Usage
	seenData, seenMetadata := false, false

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()

		if m := chunkLine.FindStringSubmatch(line); m != nil {
			size, err := strconv.ParseUint(m[2], 10, 64)
			if err != nil {
				return Usage{}, fmt.Errorf("blockvol: parse chunk size in %q: %w", line, err)
			}

			used, err := strconv.ParseUint(m[3], 10, 64)
			if err != nil {
				return Usage{}, fmt.Errorf("blockvol: parse chunk used in %q: %w", line, err)
			}

			switch m[1] {
			case "Data":
				u.DataSize, u.DataUsed, seenData = size, used, true
			case "Metadata":
				u.MetadataSize, u.MetadataUsed, seenMetadata = size, used, true
			case "System":
				u.SystemSize, u.SystemUsed = size, used
			}

			continue
		}

		m := overallLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		key, value := strings.TrimSpace(m[1]), m[2]

		switch key {
		case "Device size":
			u.DeviceSize = parseUintOrZero(value)
		case "Device unallocated":
			u.DeviceUnallocated = parseUintOrZero(value)
		case "Free (estimated)":
			u.FreeEstimated = parseUintOrZero(value)
		case "Global reserve":
			u.GlobalReserve = parseUintOrZero(value)
		case "Data ratio":
			u.DataRatio, _ = strconv.ParseFloat(value, 64)
		case "Metadata ratio":
			u.MetadataRatio, _ = strconv.ParseFloat(value, 64)
		}
	}

	if err := scanner.Err(); err != nil {
		return Usage{}, fmt.Errorf("blockvol: read usage output: %w", err)
	}

	if !seenData || !seenMetadata || u.DeviceSize == 0 {
		return Usage{}, errors.New("blockvol: `btrfs filesystem usage -b` output is missing Data/Metadata chunk lines or device size")
	}

	return u, nil
}

func parseUintOrZero(s string) uint64 {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}

	return v
}

// Check refuses when the operation would push either pool under its
// watermark. requiredData is the data the operation will materialise up
// front (the fallocate of Create); clones and seals pass 0 because Btrfs
// shares their blocks until the guest writes.
func (w Watermarks) Check(u Usage, requiredData uint64) error {
	if free := u.DataFree(); free < requiredData+w.MinDataFree {
		return &CapacityError{Pool: "data", Free: free, Required: requiredData, Watermark: w.MinDataFree}
	}

	if free := u.MetadataFree(); free < w.MinMetadataFree {
		return &CapacityError{Pool: "metadata", Free: free, Required: 0, Watermark: w.MinMetadataFree}
	}

	return nil
}
