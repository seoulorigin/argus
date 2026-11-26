//go:build testing

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsAppRoute(t *testing.T) {
	tests := []struct {
		path     string
		basePath string
		want     bool
	}{
		// known routes
		{"/", "/", true},
		{"/containers", "/", true},
		{"/containers/", "/", true},
		{"/Containers", "/", true},
		{"/smart", "/", true},
		{"/forgot-password", "/", true},
		{"/request-otp", "/", true},
		{"/system/abc123", "/", true},
		{"/system/abc123/", "/", true},
		{"/settings", "/", true},
		{"/settings/general", "/", true},

		// unknown paths
		{"/.env", "/", false},
		{"/phpinfo.php", "/", false},
		{"/wp-admin/", "/", false},
		{"/.git/config", "/", false},
		{"/system", "/", false},
		{"/system/", "/", false},
		{"/system/abc/def", "/", false},
		{"/settings/general/extra", "/", false},
		{"/containersx", "/", false},

		// base path, prefix not stripped by proxy
		{"/argus", "/argus/", true},
		{"/argus/", "/argus/", true},
		{"/argus/containers", "/argus/", true},
		{"/argus/system/abc123", "/argus/", true},
		{"/argus/.env", "/argus/", false},
		{"/argusx", "/argus/", false},

		// base path, prefix stripped by proxy
		{"/", "/argus/", true},
		{"/containers", "/argus/", true},
		{"/.env", "/argus/", false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, isAppRoute(tt.path, tt.basePath), "path=%q basePath=%q", tt.path, tt.basePath)
	}
}

func TestGetPublicAppInfoAutoLogin(t *testing.T) {
	tests := []struct {
		name          string
		autoLogin     string
		trustedHeader string
		want          bool
	}{
		{"neither configured", "", "", false},
		{"auto login", "user@example.com", "", true},
		{"trusted header", "", "X-Email", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// prefixed vars take precedence, so an empty value overrides any ambient env
			t.Setenv("ARGUS_HUB_AUTO_LOGIN", tt.autoLogin)
			t.Setenv("ARGUS_HUB_TRUSTED_AUTH_HEADER", tt.trustedHeader)
			info := getPublicAppInfo(&Hub{appURL: "http://localhost:8090"})
			assert.Equal(t, tt.want, info.AUTO_LOGIN)
		})
	}
}
