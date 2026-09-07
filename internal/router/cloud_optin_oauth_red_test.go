package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authadapter "dev.helix.agent/internal/adapters/auth"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HA-F2-002 (round-8 review remediation) — the OAuth-session half of the cloud
// opt-in gate.
//
// The gate shipped in the previous round covered four implicit
// cloud-acquisition sites and DISCLOSED this fifth one as deliberately
// excluded, on the reasoning that "`claude login` is an operator decision".
// That reasoning does not survive contact with the code:
// authadapter.GetOAuthCredentialPaths only os.Stat()s
// ~/.claude/.credentials.json and ~/.qwen/oauth_creds.json, so FILE PRESENCE
// ALONE started a 5-minute refresh ticker whose RefreshAll POSTs
// grant_type=refresh_token to api.anthropic.com / dashscope.aliyuncs.com. The
// login that produced that file authorised Claude Code, not HelixAgent, and an
// ambient file on disk is not the operator asking THIS service to reach a
// third-party endpoint — which is precisely the distinction every other gated
// site is gated on.
//
// Like the sibling guards in internal/services and internal/verifier, this
// test observes the ROUTE and not a flag: it counts outbound requests through
// a swapped http.DefaultTransport (the refresher's client has a nil Transport,
// so it resolves to the default) after driving the production sequence.
//
//	RED_MODE=1   — assert the DEFECT IS PRESENT: the pre-fix sequence
//	               (GetOAuthCredentialPaths -> NewOAuthCredentialManager ->
//	               RefreshAll, verbatim what SetupRouterWithContext used to
//	               inline) issues a real token-endpoint request from nothing
//	               but two files on disk, with HELIX_CLOUD_PROVIDERS unset.
//	default (=0) — the standing GREEN guard: no manager, no ticker, zero
//	               outbound requests.
//
// §11.4.115 RED-baseline-on-the-broken-artifact + polarity switch.
// §11.4.135 standing regression guard.
func TestCloudGate_OAuthRefreshDoesNotStartWithoutOptIn(t *testing.T) {
	calls := installCountingTransportForTest(t)
	seedOAuthCredentialFilesForTest(t)
	t.Setenv("HELIX_CLOUD_PROVIDERS", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if os.Getenv("RED_MODE") == "1" {
		// Verbatim pre-fix sequence: no gate anywhere in it.
		paths := authadapter.GetOAuthCredentialPaths()
		require.NotEmpty(t, paths,
			"RED_MODE=1: the seeded credential files must be discovered by presence alone")
		manager, err := authadapter.NewOAuthCredentialManager(paths, "helixagent", quietLoggerForTest())
		require.NoError(t, err)
		require.NotNil(t, manager,
			"RED_MODE=1: defect — two files on disk are enough to construct the refresh manager")
		manager.Start(ctx)
		_ = manager.RefreshAll(ctx)

		assert.Positive(t, calls.Load(),
			"RED_MODE=1: defect — a real OAuth token-refresh request must leave the host with HELIX_CLOUD_PROVIDERS unset")
		assert.Positive(t, calls.tokenEndpointHits(),
			"RED_MODE=1: defect — that request must target a third-party OAuth token endpoint")
		return
	}

	manager := newOAuthCredentialManager(ctx, false, quietLoggerForTest())
	assert.Nil(t, manager,
		"no OAuth refresh manager may be constructed (and therefore no 5-minute ticker started) while HELIX_CLOUD_PROVIDERS is unset")

	// Drive the same production shape a caller would; with the gate closed
	// there is nothing to drive, so nothing may leave the host.
	if manager != nil {
		_ = manager.RefreshAll(ctx)
	}
	assert.Zero(t, calls.Load(),
		"ZERO outbound requests may be issued from ambient OAuth credential files while HELIX_CLOUD_PROVIDERS is unset")
}

// TestCloudGate_OAuthRefreshStartsWithOptIn is the other polarity, and it is
// what makes the assertion above meaningful: it proves the zero-request result
// is the GATE's doing and not a dead code path. With the opt-in set, the very
// same seeded files produce a manager that really does reach a token endpoint.
func TestCloudGate_OAuthRefreshStartsWithOptIn(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: HA-F2-002 opt-in seam is asserted by the GREEN guard; the RED evidence is the defect reproduction above")
	}

	calls := installCountingTransportForTest(t)
	seedOAuthCredentialFilesForTest(t)
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := newOAuthCredentialManager(ctx, false, quietLoggerForTest())
	require.NotNil(t, manager,
		"with HELIX_CLOUD_PROVIDERS=true the operator asked for cloud: their claude/qwen OAuth session must still be refreshed")

	_ = manager.RefreshAll(ctx)
	assert.Positive(t, calls.tokenEndpointHits(),
		"with the opt-in set, the refresh must still reach the provider's OAuth token endpoint")
}

// TestCloudGate_OAuthRefreshStaysOffInStandaloneMode pins the pre-existing
// standalone-mode exclusion, so the new gate is proven to be an ADDITIONAL
// condition rather than a replacement for one that already held.
func TestCloudGate_OAuthRefreshStaysOffInStandaloneMode(t *testing.T) {
	if os.Getenv("RED_MODE") == "1" {
		t.Skip("SKIP-OK: documents a boundary that already held pre-fix; nothing to reproduce")
	}

	calls := installCountingTransportForTest(t)
	seedOAuthCredentialFilesForTest(t)
	t.Setenv("HELIX_CLOUD_PROVIDERS", "true")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := newOAuthCredentialManager(ctx, true /* standaloneMode */, quietLoggerForTest())
	assert.Nil(t, manager, "standalone mode must keep the refresh loop off even with the cloud opt-in set")
	assert.Zero(t, calls.Load(), "standalone mode must issue no OAuth refresh requests")
}

// --- helpers -----------------------------------------------------------------

type countingTransport struct {
	total  atomic.Int64
	tokens atomic.Int64
}

func (c *countingTransport) Load() int64              { return c.total.Load() }
func (c *countingTransport) tokenEndpointHits() int64 { return c.tokens.Load() }

// RoundTrip records the attempt and answers locally. It never dials: a guard
// that reached the real api.anthropic.com would be an integration test against
// a third party (CONST-035), and the observable under test is the ATTEMPT.
func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.total.Add(1)
	switch req.URL.Host {
	case "api.anthropic.com", "dashscope.aliyuncs.com":
		c.tokens.Add(1)
	}
	body := `{"access_token":"refreshed-not-a-real-token","refresh_token":"r","expires_in":3600,"token_type":"Bearer"}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// installCountingTransportForTest swaps http.DefaultTransport for the duration
// of t. NewOAuthCredentialManager builds its refresher with
// &http.Client{Timeout: 30s} — a nil Transport, which resolves to
// http.DefaultTransport — so this observes every request that path makes.
func installCountingTransportForTest(t *testing.T) *countingTransport {
	t.Helper()
	ct := &countingTransport{}
	original := http.DefaultTransport
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = original })
	return ct
}

// seedOAuthCredentialFilesForTest points HOME at a temp dir holding the two
// files GetOAuthCredentialPaths stats. They carry the FLAT schema the generic
// reader understands, with an already-past expiry, so NeedsRefresh fires and
// the refresh really is attempted — otherwise "zero requests" would prove
// nothing about the gate.
func seedOAuthCredentialFilesForTest(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	write := func(dir, name string) {
		require.NoError(t, os.MkdirAll(filepath.Join(home, dir), 0o700))
		payload, err := json.Marshal(map[string]any{
			"access_token":  "not-a-real-token-fixture-only",
			"refresh_token": "not-a-real-refresh-token-fixture-only",
			"expires_at":    time.Now().Add(-time.Hour).Format(time.RFC3339),
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(home, dir, name), payload, 0o600))
	}
	write(".claude", ".credentials.json")
	write(".qwen", "oauth_creds.json")

	// Sanity: the fixture must actually be discoverable, or every assertion
	// below would pass vacuously.
	require.Len(t, authadapter.GetOAuthCredentialPaths(), 2,
		"fixture must be discovered by GetOAuthCredentialPaths, otherwise the guard is vacuous")
}

func quietLoggerForTest() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.PanicLevel)
	return l
}
