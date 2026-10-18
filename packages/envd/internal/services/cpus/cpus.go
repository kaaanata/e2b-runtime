// Package cpus brings guest CPUs online and offline for CPU hot resize.
//
// A CPU-hotplug template's VM is created with more vCPUs than it runs with and
// boots with maxcpus=<base>: the extra CPUs are present but offline. Set makes
// exactly n CPUs online — the lowest-numbered present ones — through
// /sys/devices/system/cpu/cpuN/online. The host cgroup quota of the
// Firecracker process enforces the count; this only makes the guest's view
// (nproc, sysconf, the scheduler) match it.
package cpus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// DefaultRoot is the sysfs CPU directory.
const DefaultRoot = "/sys/devices/system/cpu"

// ErrOutOfRange reports a request outside [1, present CPUs].
var ErrOutOfRange = errors.New("requested CPU count is out of range")

// State is the guest's CPU state after a call.
type State struct {
	Online       int64
	Present      int64
	Hotpluggable bool
	Changed      int64
	Errors       []string
}

// Controller changes CPU online state under one sysfs root. Calls are
// serialized.
type Controller struct {
	root string
	mu   sync.Mutex
}

// New returns a Controller for root (DefaultRoot when empty).
func New(root string) *Controller {
	if root == "" {
		root = DefaultRoot
	}

	return &Controller{root: root}
}

// ParseList parses a kernel CPU list ("0-3,8,10-11").
func ParseList(text string) ([]int, error) {
	var out []int
	for part := range strings.SplitSeq(strings.TrimSpace(text), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("parse CPU list %q: %w", text, err)
		}

		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("parse CPU list %q: %w", text, err)
			}
		}

		if last < first {
			return nil, fmt.Errorf("parse CPU list %q: descending range", text)
		}

		for c := first; c <= last; c++ {
			out = append(out, c)
		}
	}

	slices.Sort(out)

	return slices.Compact(out), nil
}

func (c *Controller) readList(name string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(c.root, name))
	if err != nil {
		return nil, err
	}

	return ParseList(string(data))
}

func (c *Controller) onlinePath(cpu int) string {
	return filepath.Join(c.root, fmt.Sprintf("cpu%d", cpu), "online")
}

// Get reports the present and online CPUs.
func (c *Controller) Get() (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	present, err := c.readList("present")
	if err != nil {
		return State{}, err
	}

	return c.state(present, 0, nil)
}

func (c *Controller) state(present []int, changed int64, errs []string) (State, error) {
	online, err := c.readList("online")
	if err != nil {
		return State{}, err
	}

	hotpluggable := true
	for _, cpu := range present {
		if cpu == 0 {
			continue
		}
		if _, err := os.Stat(c.onlinePath(cpu)); err != nil {
			hotpluggable = false

			break
		}
	}

	return State{
		Online:       int64(len(online)),
		Present:      int64(len(present)),
		Hotpluggable: hotpluggable,
		Changed:      changed,
		Errors:       errs,
	}, nil
}

// Set makes exactly n CPUs online: it onlines the lowest-numbered present CPUs
// up to n (ascending) and offlines the rest (descending), writing only CPUs
// not already in the wanted state. cpu0 has no control and stays online. A
// CPU that cannot be changed is reported in State.Errors; the others are still
// changed.
func (c *Controller) Set(n int64) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	present, err := c.readList("present")
	if err != nil {
		return State{}, err
	}

	if n < 1 || n > int64(len(present)) {
		return State{}, fmt.Errorf("%w: %d is outside [1, %d]", ErrOutOfRange, n, len(present))
	}

	online, err := c.readList("online")
	if err != nil {
		return State{}, err
	}

	var (
		changed int64
		errs    []string
	)

	apply := func(cpu int, want bool) {
		if cpu == 0 || slices.Contains(online, cpu) == want {
			return
		}

		val := "0"
		if want {
			val = "1"
		}

		if err := writeControl(c.onlinePath(cpu), val); err != nil {
			errs = append(errs, fmt.Sprintf("cpu%d online=%s: %v", cpu, val, err))

			return
		}

		changed++
	}

	for _, cpu := range present[:n] {
		apply(cpu, true)
	}

	for i := len(present) - 1; i >= int(n); i-- {
		apply(present[i], false)
	}

	return c.state(present, changed, errs)
}

// writeControl writes an existing sysfs control file; it never creates one (a
// CPU without a control cannot be hotplugged).
func writeControl(path, val string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}

	_, werr := f.WriteString(val)
	cerr := f.Close()

	return errors.Join(werr, cerr)
}
