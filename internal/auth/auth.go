package auth

import (
	"strings"

	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/gocommon/grpcserver"
	"github.com/huponx/gocommon/jwtx"
	"go.uber.org/zap"
)

// isLocal reports whether courier runs on a developer machine. Only ENV=local has optional
// auth; every other value (dev, staging, production, empty, a typo) requires it.
func isLocal(env string) bool {
	return strings.EqualFold(strings.TrimSpace(env), "local")
}

// authSettings decides how gRPC calls are authenticated and which warnings to log.
//
//   - ENV=local: services are not authenticated (there is no Kubernetes API server on a laptop to review
//     tokens); users still need their token for the inbox and the admin screens.
//   - any other ENV: ServiceAccount tokens reviewed by the API server. An empty COURIER_AUTH_ALLOWED is
//     valid and rejects every caller.
func authSettings(cfg appconfig.Courier) (local bool, kube grpcserver.KubeAuthConfig, warnings []string) {
	if isLocal(cfg.Env) {
		warnings = append(warnings, "ENV=local: gRPC auth of services is disabled")
		return true, grpcserver.KubeAuthConfig{}, warnings
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

// ServerOption builds the server auth option, see authSettings: a user token (JWT, for InboxService) or the
// service credential above. Method permissions are enforced on top (courier/v1 rules): a user holds none of the
// courier:notification:* permissions, so a user token reaches the inbox only. verifier may be nil (no
// COURIER_ISSUER): then only services are accepted. Outside ENV=local it panics (inside KubeAuth) when the
// audience is empty or the cluster CA cannot be read, so the process cannot start unprotected.
func ServerOption(cfg appconfig.Courier, log *zap.Logger, verifier *jwtx.Verifier) grpcserver.Option {
	local, kube, warnings := authSettings(cfg)
	for _, w := range warnings {
		log.Warn(w)
	}
	var fns []grpcserver.AuthFunc
	if verifier != nil {
		fns = append(fns, grpcserver.JWTAuth(verifier)) // cheap and local: before the TokenReview of KubeAuth
	}
	if local {
		fns = append(fns, grpcserver.LocalCaller("local"))
	} else {
		k, err := grpcserver.KubeAuth(kube)
		if err != nil {
			panic(err)
		}
		fns = append(fns, k)
	}
	return grpcserver.WithAuth(grpcserver.AnyAuth(fns...))
}
