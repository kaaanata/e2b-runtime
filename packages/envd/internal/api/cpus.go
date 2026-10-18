package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/e2b-dev/infra/packages/envd/internal/logs"
	"github.com/e2b-dev/infra/packages/envd/internal/services/cpus"
)

func cpusResult(st cpus.State) CPUsResult {
	res := CPUsResult{
		Online:       st.Online,
		Present:      st.Present,
		Hotpluggable: st.Hotpluggable,
		Changed:      &st.Changed,
	}
	if len(st.Errors) > 0 {
		res.Errors = &st.Errors
	}

	return res
}

// GetCpus reports the guest's present and online CPUs.
func (a *API) GetCpus(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	st, err := a.cpus.Get()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)

		return
	}

	writeCPUsResult(w, st)
}

// PostCpus makes exactly the requested number of CPUs online (CPU hot resize).
// The orchestrator calls it after it moved the host cgroup quota, which is what
// enforces the count.
func (a *API) PostCpus(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	logger := a.logger.With().Str(string(logs.OperationIDKey), logs.AssignOperationID()).Logger()

	var req CPUsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, err)

		return
	}

	st, err := a.cpus.Set(req.Online)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, cpus.ErrOutOfRange) {
			code = http.StatusBadRequest
		}
		logger.Error().Err(err).Int64("online", req.Online).Msg("set online CPUs")
		jsonError(w, code, err)

		return
	}

	if len(st.Errors) > 0 {
		logger.Warn().Strs("errors", st.Errors).Int64("online", st.Online).Int64("requested", req.Online).Msg("some CPUs could not be changed")
	} else {
		logger.Info().Int64("online", st.Online).Int64("changed", st.Changed).Msg("set online CPUs")
	}

	writeCPUsResult(w, st)
}

func writeCPUsResult(w http.ResponseWriter, st cpus.State) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cpusResult(st)); err != nil {
		// The status line is already out; nothing left to report to the client.
		return
	}
}
