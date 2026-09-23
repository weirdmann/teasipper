package teasipper

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Mode selects whether an endpoint listens for connections or dials one.
type Mode string

const (
	ModeClient Mode = "client"
	ModeServer Mode = "server"
)

var (
	ErrClosed        = errors.New("teasipper: endpoint closed")
	ErrRunning       = errors.New("teasipper: endpoint already running")
	ErrNotRunning    = errors.New("teasipper: endpoint not running")
	ErrWrongMode     = errors.New("teasipper: operation unavailable in this mode")
	ErrSessionClosed = errors.New("teasipper: session closed")
)

// Config is copied at construction and reset. Active sessions never observe
// changes to a Config held by the caller.
type Config struct {
	Mode            Mode
	Network         string        // "tcp" (default), "tcp4", or "tcp6"
	Address         string        // listen address in server mode; remote address in client mode
	LocalAddress    string        // optional client bind address
	DialTimeout     time.Duration // client connection setup; zero uses the context only
	ReadTimeout     time.Duration // per Read call; zero means no timeout
	WriteTimeout    time.Duration // per Write call; zero means no timeout
	NoDelay         bool          // true explicitly enables TCP_NODELAY; false keeps Go's default
	KeepAlivePeriod time.Duration // positive enables keepalive with this period; zero keeps Go's default
}

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.Mode != ModeClient && cfg.Mode != ModeServer {
		return Config{}, fmt.Errorf("teasipper: invalid mode %q", cfg.Mode)
	}
	if cfg.Network == "" {
		cfg.Network = "tcp"
	}
	if cfg.Network != "tcp" && cfg.Network != "tcp4" && cfg.Network != "tcp6" {
		return Config{}, fmt.Errorf("teasipper: unsupported network %q", cfg.Network)
	}
	if strings.TrimSpace(cfg.Address) == "" {
		return Config{}, errors.New("teasipper: address is required")
	}
	if cfg.Mode == ModeServer && cfg.LocalAddress != "" {
		return Config{}, errors.New("teasipper: LocalAddress is only valid in client mode")
	}
	if cfg.LocalAddress != "" {
		host, port, err := net.SplitHostPort(cfg.LocalAddress)
		if err != nil {
			return Config{}, fmt.Errorf("teasipper: invalid LocalAddress: %w", err)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 0 || portNumber > 65535 {
			return Config{}, errors.New("teasipper: LocalAddress port must be numeric and between 0 and 65535")
		}
		ip := host
		if zone := strings.LastIndexByte(ip, '%'); zone >= 0 {
			ip = ip[:zone]
		}
		if host != "" && net.ParseIP(ip) == nil {
			return Config{}, errors.New("teasipper: LocalAddress host must be a numeric IP")
		}
	}
	if cfg.DialTimeout < 0 || cfg.ReadTimeout < 0 || cfg.WriteTimeout < 0 {
		return Config{}, errors.New("teasipper: timeouts must not be negative")
	}
	if cfg.KeepAlivePeriod < 0 {
		return Config{}, errors.New("teasipper: KeepAlivePeriod must not be negative")
	}
	return cfg, nil
}
