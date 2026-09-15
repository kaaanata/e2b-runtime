package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/dashboard-api/internal/api"
	dashboardqueries "github.com/e2b-dev/infra/packages/db/pkg/dashboard/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/ginutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

func (s *APIStore) PatchTeamsTeamID(c *gin.Context, teamID api.TeamID) {
	ctx := c.Request.Context()
	telemetry.ReportEvent(ctx, "update team")

	authTeamID, ok := s.requireAuthedTeamMatchesPath(c, teamID)
	if !ok {
		return
	}

	telemetry.SetAttributes(ctx, telemetry.WithTeamID(authTeamID.String()))

	body, err := ginutils.ParseBodyWith(ctx, c, parseUpdateTeamBody)
	if err != nil {
		s.sendAPIStoreError(c, http.StatusBadRequest, "Invalid request body")

		return
	}

	if !body.NameSet {
		s.sendAPIStoreError(c, http.StatusBadRequest, "At least one field must be provided")

		return
	}

	if body.NameSet && strings.TrimSpace(body.Name) == "" {
		s.sendAPIStoreError(c, http.StatusBadRequest, "Name must not be empty")

		return
	}

	row, err := s.db.Dashboard.UpdateTeam(ctx, dashboardqueries.UpdateTeamParams{
		TeamID:  authTeamID,
		Name:    body.NamePtr(),
		NameSet: body.NameSet,
	})
	if err != nil {
		logger.L().Error(ctx, "failed to update team", zap.Error(err), logger.WithTeamID(authTeamID.String()))
		s.sendAPIStoreError(c, http.StatusInternalServerError, "Failed to update team")

		return
	}

	c.JSON(http.StatusOK, api.UpdateTeamResponse{
		Id:   row.ID,
		Name: row.Name,
	})
}

type updateTeamBody struct {
	NameSet bool
	Name    string
}

func (b updateTeamBody) NamePtr() *string {
	if !b.NameSet {
		return nil
	}

	return &b.Name
}

func parseUpdateTeamBody(bodyReader io.Reader) (updateTeamBody, error) {
	var body updateTeamBody

	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(bodyReader)
	if err := decoder.Decode(&payload); err != nil {
		return body, err
	}

	for field := range payload {
		if field != "name" {
			return body, errors.New("unknown field")
		}
	}

	nameRaw, hasName := payload["name"]
	if hasName {
		body.NameSet = true
		if bytes.Equal(nameRaw, []byte("null")) {
			return body, errors.New("name cannot be null")
		}

		var name string
		if err := json.Unmarshal(nameRaw, &name); err != nil {
			return body, err
		}

		body.Name = name
	}

	return body, nil
}
