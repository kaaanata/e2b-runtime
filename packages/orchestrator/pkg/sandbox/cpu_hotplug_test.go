//go:build linux

package sandbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/cgroup"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/envd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
)

func cpuLayout() *metadata.CPUHotplugLayout {
	return &metadata.CPUHotplugLayout{BaseVCPUs: 8, MaxVCPUs: 32}
}

func TestConfig_VCPUDefaults(t *testing.T) {
	t.Parallel()

	fixed := NewConfig(Config{Vcpu: 4})
	assert.Equal(t, int64(4), fixed.RequestedVCPUs())
	assert.Equal(t, int64(4), fixed.OnlineVCPUs())
	assert.Equal(t, int64(4), fixed.MaxVCPUs())
	assert.Equal(t, int64(4), fixed.AccountedVCPUs())

	hot := NewConfig(Config{Vcpu: 8, CPUHotplug: cpuLayout()}).WithRequestedVCPUs(16)
	assert.Equal(t, int64(16), hot.RequestedVCPUs())
	assert.Equal(t, int64(16), hot.AccountedVCPUs(), "the granted count is what the quota allows")
	assert.Equal(t, int64(32), hot.MaxVCPUs())

	hot.setVCPUs(16, 12)
	assert.Equal(t, int64(12), hot.OnlineVCPUs())
	hot.setVCPUs(4, 0)
	assert.Equal(t, int64(12), hot.OnlineVCPUs(), "0 keeps the last observed count")
}

func TestConfig_AdoptCPUHotplug(t *testing.T) {
	t.Parallel()

	withLayout := metadata.Template{}.WithCPUHotplug(cpuLayout()).MarkCPUHotplugVCPUs(12)
	fixed := metadata.Template{}

	t.Run("resume without a request keeps the granted count", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{Vcpu: 8})
		require.NoError(t, c.adoptCPUHotplug(withLayout))
		assert.Equal(t, int64(12), c.RequestedVCPUs())
		assert.Equal(t, cpuLayout(), c.CPUHotplug)
	})

	t.Run("template snapshot starts at the base", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{Vcpu: 8})
		require.NoError(t, c.adoptCPUHotplug(metadata.Template{}.WithCPUHotplug(cpuLayout())))
		assert.Equal(t, int64(8), c.RequestedVCPUs())
	})

	t.Run("explicit request within range", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{Vcpu: 8}).WithRequestedVCPUs(32)
		require.NoError(t, c.adoptCPUHotplug(withLayout))
		assert.Equal(t, int64(32), c.RequestedVCPUs())
	})

	t.Run("request out of range", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{Vcpu: 8}).WithRequestedVCPUs(33)
		require.ErrorIs(t, c.adoptCPUHotplug(withLayout), metadata.ErrCPUCountOutOfRange)
	})

	t.Run("base mismatch", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{Vcpu: 4})
		require.Error(t, c.adoptCPUHotplug(withLayout))
	})

	t.Run("fixed template refuses another count", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{Vcpu: 8}).WithRequestedVCPUs(16)
		require.ErrorIs(t, c.adoptCPUHotplug(fixed), ErrCPUNotHotResizable)

		same := NewConfig(Config{Vcpu: 8}).WithRequestedVCPUs(8)
		require.NoError(t, same.adoptCPUHotplug(fixed))
	})

	t.Run("a layout the snapshot lacks", func(t *testing.T) {
		t.Parallel()

		c := NewConfig(Config{Vcpu: 8, CPUHotplug: cpuLayout()})
		require.ErrorIs(t, c.adoptCPUHotplug(fixed), ErrCPUNotHotResizable)
	})
}

func TestConfig_ApplyCPUQuota(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	h := cgroup.HandleAtPath(dir)

	require.NoError(t, NewConfig(Config{Vcpu: 4}).applyCPUQuota(h), "fixed: no quota")
	_, err := os.Stat(filepath.Join(dir, "cpu.max"))
	require.True(t, os.IsNotExist(err), "a fixed-size sandbox's cgroup is left alone")

	hot := NewConfig(Config{Vcpu: 8, CPUHotplug: cpuLayout()}).WithRequestedVCPUs(16)
	require.NoError(t, hot.applyCPUQuota(h))
	q, err := h.CPUQuota()
	require.NoError(t, err)
	assert.InDelta(t, 16.0, q, 1e-9)

	noop, err := cgroup.NewNoopManager().Create(t.Context(), "sbx-x")
	require.NoError(t, err)
	require.ErrorIs(t, hot.applyCPUQuota(noop), cgroup.ErrNoCPUQuota, "no cgroup, no enforcement: refuse the start")
}

// fakeCPUsEnvd answers /cpus like envd on a guest with present CPUs, recording
// the order of quota changes relative to the online changes.
type fakeCPUsEnvd struct {
	mu      sync.Mutex
	present int64
	online  int64
	calls   []int64
	quotaAt []float64
	handle  *cgroup.CgroupHandle
}

func (f *fakeCPUsEnvd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path != "/cpus" {
		http.NotFound(w, r)

		return
	}

	if r.Method == http.MethodPost {
		var req envd.CPUsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		f.calls = append(f.calls, req.Online)
		q, _ := f.handle.CPUQuota()
		f.quotaAt = append(f.quotaAt, q)
		f.online = req.Online
	}

	if err := json.NewEncoder(w).Encode(envd.CPUsResult{Online: f.online, Present: f.present, Hotpluggable: true}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func newCPUSandbox(t *testing.T, envdURL string, h *cgroup.CgroupHandle) *Sandbox {
	t.Helper()

	token := "test-token"
	s := &Sandbox{Metadata: &Metadata{
		Config: NewConfig(Config{
			Vcpu:       8,
			CPUHotplug: cpuLayout(),
			Envd:       EnvdMetadata{AccessToken: &token},
		}),
		Runtime: RuntimeMetadata{SandboxID: "test-sandbox"},
	}}
	s.internalConfig.envdServerURLOverride = envdURL
	s.cgroupHandle = h

	return s
}

func TestResizeCPU_QuotaFirstThenGuest(t *testing.T) {
	t.Parallel()

	h := cgroup.HandleAtPath(t.TempDir())
	fake := &fakeCPUsEnvd{present: 32, online: 8, handle: h}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	s := newCPUSandbox(t, srv.URL, h)

	res, err := s.ResizeCPU(t.Context(), 16)
	require.NoError(t, err)
	assert.Equal(t, CPUResizeResult{VCPUs: 16, OnlineVCPUs: 16, MaxVCPUs: 32, Complete: true}, res)

	res, err = s.ResizeCPU(t.Context(), 8)
	require.NoError(t, err)
	assert.True(t, res.Complete)

	assert.Equal(t, []int64{16, 8}, fake.calls)
	assert.Equal(t, []float64{16, 8}, fake.quotaAt, "the quota already holds when the guest is asked (both directions)")
	assert.Equal(t, int64(8), s.Config.AccountedVCPUs())

	_, err = s.ResizeCPU(t.Context(), 33)
	require.ErrorIs(t, err, metadata.ErrCPUCountOutOfRange)
	_, err = s.ResizeCPU(t.Context(), 7)
	require.ErrorIs(t, err, metadata.ErrCPUCountOutOfRange)
}

func TestResizeCPU_EnvdFailureKeepsTheQuota(t *testing.T) {
	t.Parallel()

	h := cgroup.HandleAtPath(t.TempDir())
	srv := httptest.NewServer(http.NotFoundHandler()) // an envd without /cpus
	t.Cleanup(srv.Close)

	s := newCPUSandbox(t, srv.URL, h)

	res, err := s.ResizeCPU(t.Context(), 16)
	require.NoError(t, err, "the grant took effect; the guest's view is reported incomplete")
	assert.False(t, res.Complete)
	assert.Equal(t, int64(16), res.VCPUs)

	q, err := h.CPUQuota()
	require.NoError(t, err)
	assert.InDelta(t, 16.0, q, 1e-9)
}

func TestResizeCPU_FixedTemplate(t *testing.T) {
	t.Parallel()

	s := &Sandbox{Metadata: &Metadata{Config: NewConfig(Config{Vcpu: 8})}}
	_, err := s.ResizeCPU(t.Context(), 16)
	require.ErrorIs(t, err, ErrCPUNotHotResizable)
}

func TestVerifyCPUHotplug(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		present, online int64
		hotpluggable    bool
		stuck           bool // the guest refuses to change
		code            int
		ok              bool
	}{
		"booted the base, rest present":   {present: 32, online: 8, hotpluggable: true, ok: true},
		"udev onlined everything at boot": {present: 32, online: 32, hotpluggable: true, ok: true},
		"no hotplug control":              {present: 32, online: 8},
		"nr_cpus instead of maxcpus":      {present: 8, online: 8, hotpluggable: true},
		"the guest cannot reach the base": {present: 32, online: 32, hotpluggable: true, stuck: true},
		"envd without /cpus":              {code: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			online := tc.online
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.code != 0 {
					w.WriteHeader(tc.code)

					return
				}

				mu.Lock()
				defer mu.Unlock()

				if r.Method == http.MethodPost && !tc.stuck {
					var req envd.CPUsRequest
					_ = json.NewDecoder(r.Body).Decode(&req)
					online = req.Online
				}
				_ = json.NewEncoder(w).Encode(envd.CPUsResult{Online: online, Present: tc.present, Hotpluggable: tc.hotpluggable})
			}))
			t.Cleanup(srv.Close)

			err := newCPUSandbox(t, srv.URL, nil).VerifyCPUHotplug(t.Context())
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, int64(8), online, "the layer snapshot holds the base online")
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestStampCPUHotplugVCPUs(t *testing.T) {
	t.Parallel()

	s := &Sandbox{Metadata: &Metadata{Config: NewConfig(Config{Vcpu: 8, CPUHotplug: cpuLayout()}).WithRequestedVCPUs(24)}}
	assert.Equal(t, int64(24), s.stampCPUHotplugVCPUs(metadata.Template{}).CPUHotplugVCPUs)

	fixed := &Sandbox{Metadata: &Metadata{Config: NewConfig(Config{Vcpu: 8})}}
	assert.Zero(t, fixed.stampCPUHotplugVCPUs(metadata.Template{}.MarkCPUHotplugVCPUs(9)).CPUHotplugVCPUs)
}
