package router

import (
	"context"

	authadapter "dev.helix.agent/internal/adapters/auth"
	"dev.helix.agent/internal/localfirst"
	"github.com/sirupsen/logrus"
)

// newOAuthCredentialManager builds the OAuth credential manager for the
// providers that authenticate by OAuth session rather than by API key
// (Claude, Qwen). It returns nil when no manager should run, in which case no
// refresh ticker is started and no token endpoint is ever contacted.
//
// Local-first default (spec 002, HA-F2-002). This is the FIFTH implicit
// cloud-acquisition site, and it is gated by the SAME predicate as the other
// four (internal/localfirst).
//
// Why it needed gating even though the credentials are the operator's own:
// authadapter.GetOAuthCredentialPaths() only os.Stat()s ~/.claude/.credentials.json
// and ~/.qwen/oauth_creds.json. FILE PRESENCE ALONE was enough to start a
// 5-minute ticker whose RefreshAll can POST grant_type=refresh_token to
// api.anthropic.com/oauth/token and dashscope.aliyuncs.com/api/token. The
// earlier classification — "`claude login` is an operator decision" — does not
// hold up: that login authorised *Claude Code*, not this service, and an
// ambient file on disk is not the same thing as an operator asking HelixAgent
// to reach a third-party endpoint. Every other gated site is gated on exactly
// that distinction (an ambient credential the process decided to use on its
// own), so this one belongs on the same side of the line.
//
// Nothing is taken away from the operator: HELIX_CLOUD_PROVIDERS=true restores
// the refresh loop unchanged, which is what an operator who actually wants
// HelixAgent to use their Claude/Qwen OAuth session sets anyway — without it
// no cloud route exists for the refreshed token to serve.
func newOAuthCredentialManager(
	ctx context.Context,
	standaloneMode bool,
	logger *logrus.Logger,
) *authadapter.OAuthCredentialManager {
	if standaloneMode {
		return nil
	}
	if !localfirst.CloudProvidersOptedIn() {
		logger.Debug("Cloud providers not opted in (HELIX_CLOUD_PROVIDERS unset/false): " +
			"OAuth credential refresh for claude/qwen stays disabled")
		return nil
	}

	oauthPaths := authadapter.GetOAuthCredentialPaths()
	if len(oauthPaths) == 0 {
		return nil
	}

	manager, err := authadapter.NewOAuthCredentialManager(oauthPaths, "helixagent", logger)
	if err != nil {
		logger.WithError(err).Warn("Failed to initialize OAuth credential manager")
		return nil
	}

	manager.Start(ctx)
	logger.WithField("providers", len(oauthPaths)).Info("OAuth credential manager initialized")
	return manager
}
