package auth

import (
	"strings"

	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/gocommon/grpcserver"
	"go.uber.org/zap"
)

// isLocal reports whether courier runs on a developer machine. Only ENV=local has optional
// auth; every other value (dev, staging, production, empty, a typo) requires it.
func isLocal(env string) bool {
	return strings.EqualFold(strings.TrimSpace(env), "local")
}

// authSettings decides how gRPC calls are authenticated and which warnings to log.
//
//   - ENV=local: optional static Bearer token (COURIER_S2S_TOKEN). An empty token leaves the
//     server open, there is no Kubernetes API server on a laptop to review tokens.
//   - any other ENV: ServiceAccount tokens reviewed by the API server. COURIER_S2S_TOKEN is
//     ignored. An empty COURIER_AUTH_ALLOWED is valid and rejects every caller.
func authSettings(cfg appconfig.Courier) (local bool, kube grpcserver.KubeAuthConfig, warnings []string) {
	if isLocal(cfg.Env) {
		if strings.TrimSpace(cfg.S2SToken) == "" {
			warnings = append(warnings, "ENV=local and COURIER_S2S_TOKEN is empty: gRPC auth is disabled")
		}
		return true, grpcserver.KubeAuthConfig{}, warnings
	}
	if strings.TrimSpace(cfg.S2SToken) != "" {
		warnings = append(warnings, "COURIER_S2S_TOKEN is ignored unless ENV=local: callers authenticate with ServiceAccount tokens")
	}
	allowed := make([]string, 0, len(cfg.AuthAllowed))
	for _, s := range cfg.AuthAllowed {
		if s = strings.TrimSpace(s); s != "" {
			allowed = append(allowed, s)
		}
	}
	if len(allowed) == 0 {
		warnings = append(warnings, "COURIER_AUTH_ALLOWED is empty: every gRPC caller is rejected")
	}
	return false, grpcserver.KubeAuthConfig{
		Audience:        cfg.AuthAudience,
		AllowedSubjects: allowed,
	}, warnings
}

// ServerOption builds the server auth option, see authSettings. Outside ENV=local it panics (inside
// WithKubeAuth) when the audience is empty or the cluster CA cannot be read, so the process
// cannot start unprotected.
func ServerOption(cfg appconfig.Courier, log *zap.Logger) grpcserver.Option {
	local, kube, warnings := authSettings(cfg)
	for _, w := range warnings {
		log.Warn(w)
	}
	if local {
		return grpcserver.WithOptionalBearerAuth(cfg.S2SToken)
	}
	return grpcserver.WithKubeAuth(kube)
}
