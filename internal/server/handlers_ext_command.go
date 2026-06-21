package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/cunninghamcard-bit/Attention/internal/backend"
	"github.com/cunninghamcard-bit/Attention/internal/protocol"
)

type ExtCommandDispatcher interface {
	DispatchCommand(
		ctx context.Context,
		pluginID string,
		owner string,
		sessionID string,
		envID string,
		name string,
		payload []byte,
	) ([]byte, error)
}

type extCommandRouter interface {
	Route(owner string, sessionID string, envID string) (backend.ExtCommandRouteMode, error)
}

type extHostDownError interface {
	error
	ExtHostDown() bool
}

type extCommandNotSupportedInServerError interface {
	error
	ExtCommandNotSupportedInServer() bool
}

type extCommandTimeoutError interface {
	error
	ExtCommandTimeout() bool
}

type extCommandFailedError interface {
	error
	ExtCommandFailed() bool
}

func (s *Server) handleExtCommand(w http.ResponseWriter, r *http.Request) {
	if s.opts.ExtCommands == nil {
		writeError(w, http.StatusNotFound, "plugins_unavailable", "plugin command dispatcher not configured")
		return
	}

	var req protocol.ExtCommandRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	if err := validateExtCommandRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	mode := backend.ExtCommandRouteInProcess
	if router, ok := s.opts.ExtCommands.(extCommandRouter); ok {
		var err error
		mode, err = router.Route(req.Owner, req.SessionID, req.EnvID)
		if err != nil {
			writeExtCommandError(w, err)
			return
		}
	}
	switch mode {
	case backend.ExtCommandRouteInProcess:
	case backend.ExtCommandRouteCrossProcess:
		s.dispatchExtCommandCrossProcess(w, r, req)
		return
	case backend.ExtCommandRouteUnsupported:
		writeError(w, http.StatusNotImplemented, "not_supported", "ext.command owner is not supported")
		return
	default:
		writeError(w, http.StatusNotImplemented, "not_supported", "ext.command route is not supported")
		return
	}

	result, err := s.opts.ExtCommands.DispatchCommand(
		r.Context(),
		req.PluginID,
		req.Owner,
		req.SessionID,
		req.EnvID,
		req.Name,
		req.Payload,
	)
	if err != nil {
		writeExtCommandError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, protocol.ExtCommandResponse{Result: result})
}

func validateExtCommandRequest(req protocol.ExtCommandRequest) error {
	if req.PluginID == "" {
		return errors.New("ext.command pluginId is required")
	}
	if req.Name == "" {
		return errors.New("ext.command name is required")
	}

	switch req.Owner {
	case "engine":
		return nil
	case "session":
		if req.SessionID == "" {
			return errors.New("ext.command session owner requires sessionId")
		}
		return nil
	case "environment":
		if req.EnvID == "" {
			return errors.New("ext.command environment owner requires envId")
		}
		return nil
	default:
		return fmt.Errorf("ext.command owner must be engine, session, or environment, got %q", req.Owner)
	}
}

func writeExtCommandError(w http.ResponseWriter, err error) {
	var hostDown extHostDownError
	var unsupported extCommandNotSupportedInServerError
	var timeout extCommandTimeoutError
	var failed extCommandFailedError
	switch {
	case errors.As(err, &unsupported) && unsupported.ExtCommandNotSupportedInServer():
		writeError(w, http.StatusNotImplemented, "not_supported", err.Error())
	case errors.As(err, &hostDown) && hostDown.ExtHostDown():
		writeError(w, http.StatusServiceUnavailable, "ext_host_down", err.Error())
	case errors.As(err, &timeout) && timeout.ExtCommandTimeout():
		writeError(w, http.StatusGatewayTimeout, "ext_command_timeout", err.Error())
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		writeError(w, http.StatusGatewayTimeout, "ext_command_timeout", err.Error())
	case errors.As(err, &failed) && failed.ExtCommandFailed():
		writeError(w, http.StatusUnprocessableEntity, "ext_command_failed", err.Error())
	default:
		writeError(w, http.StatusUnprocessableEntity, "ext_command_failed", err.Error())
	}
}
