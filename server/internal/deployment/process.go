package deployment

import (
	"fmt"
	"net"
	"strings"
)

// ProcessRole selects the composition root started by the server binary.
// The empty value remains the legacy all-in-one process for compatibility.
type ProcessRole string

const (
	RoleAll    ProcessRole = "all"
	RoleWeb    ProcessRole = "web"
	RoleWorker ProcessRole = "worker"
)

func ParseProcessRole(raw string) (ProcessRole, error) {
	switch ProcessRole(strings.ToLower(strings.TrimSpace(raw))) {
	case "", RoleAll:
		return RoleAll, nil
	case RoleWeb:
		return RoleWeb, nil
	case RoleWorker:
		return RoleWorker, nil
	default:
		return "", fmt.Errorf("MULTICA_PROCESS_ROLE must be all, web, or worker")
	}
}

func (r ProcessRole) RunsWeb() bool { return r == RoleAll || r == RoleWeb }

func (r ProcessRole) RunsWorker() bool { return r == RoleAll || r == RoleWorker }

func (r ProcessRole) Split() bool { return r == RoleWeb || r == RoleWorker }

// ValidateLoopbackAddr rejects wildcard and remote management listeners. The
// split-role control plane is deliberately local-only; operators reach it over
// an authenticated host channel instead of exposing another network surface.
func ValidateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("invalid worker admin address: %w", err)
	}
	ip := net.ParseIP(host)
	if host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("worker admin address must be loopback-only")
}
