//go:build testing

package hub_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	argusTests "argus/internal/tests"

	"argus/internal/migrations"
	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/apis"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	pbTests "ganigeorgiev.com/pocketbase/pocketbase/tests"
	"github.com/stretchr/testify/require"
)

// marshal to json and return an io.Reader (for use in ApiScenario.Body)
func jsonReader(v any) io.Reader {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(data)
}

type gatedReader struct {
	data    []byte
	started chan struct{}
	release chan struct{}
	offset  int
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.offset == 0 {
		close(r.started)
		<-r.release
	}
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func firstUserTestMux(t *testing.T) (*argusTests.TestHub, http.Handler) {
	t.Helper()
	hub, err := argusTests.NewTestHub(t.TempDir())
	require.NoError(t, err)
	_ = hub.StartHub()

	router, err := apis.NewRouter(hub.TestApp)
	require.NoError(t, err)
	serveEvent := &core.ServeEvent{App: hub.TestApp, Router: router}

	var handler http.Handler
	err = hub.TestApp.OnServe().Trigger(serveEvent, func(e *core.ServeEvent) error {
		var buildErr error
		handler, buildErr = e.Router.BuildMux()
		return buildErr
	})
	require.NoError(t, err)
	require.NotNil(t, handler)
	return hub, handler
}

func postFirstUser(handler http.Handler, email string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": "password123",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/argus/create-user", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestApiRoutesAuthentication(t *testing.T) {
	hub, user := argusTests.GetHubWithUser(t)
	defer hub.Cleanup()

	userToken, err := user.NewAuthToken()
	require.NoError(t, err, "Failed to create auth token")

	// Create test user and get auth token
	user2, err := argusTests.CreateUser(hub, "testuser@example.com", "password123")
	require.NoError(t, err, "Failed to create test user")
	_, err = user2.NewAuthToken()
	require.NoError(t, err, "Failed to create user2 auth token")

	adminUser, err := argusTests.CreateUserWithRole(hub, "admin@example.com", "password123", "admin")
	require.NoError(t, err, "Failed to create admin user")
	adminUserToken, err := adminUser.NewAuthToken()
	require.NoError(t, err, "Failed to create admin auth token")

	readOnlyUser, err := argusTests.CreateUserWithRole(hub, "readonly@example.com", "password123", "readonly")
	require.NoError(t, err, "Failed to create readonly user")
	readOnlyUserToken, err := readOnlyUser.NewAuthToken()
	require.NoError(t, err, "Failed to create readonly user auth token")

	superuser, err := argusTests.CreateSuperuser(hub, "superuser@example.com", "password123")
	require.NoError(t, err, "Failed to create superuser")
	superuserToken, err := superuser.NewAuthToken()
	require.NoError(t, err, "Failed to create superuser auth token")

	// Create test system
	_, err = argusTests.CreateRecord(hub, "systems", map[string]any{
		"name":  "test-system",
		"users": []string{user.Id, readOnlyUser.Id},
		"host":  "127.0.0.1",
	})
	require.NoError(t, err, "Failed to create test system")

	testAppFactory := func(t testing.TB) *pbTests.TestApp {
		return hub.TestApp
	}

	scenarios := []argusTests.ApiScenario{
		// Auth Protected Routes - Should require authentication
		{
			Name:            "GET /config-yaml - no auth should fail",
			Method:          http.MethodGet,
			URL:             "/api/argus/config-yaml",
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /config-yaml - with user auth should fail",
			Method: http.MethodGet,
			URL:    "/api/argus/config-yaml",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  403,
			ExpectedContent: []string{"The authorized record is not allowed to perform this action."},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /config-yaml - with admin auth should succeed",
			Method: http.MethodGet,
			URL:    "/api/argus/config-yaml",
			Headers: map[string]string{
				"Authorization": adminUserToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{"test-system"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:            "GET /universal-token - no auth should fail",
			Method:          http.MethodGet,
			URL:             "/api/argus/universal-token",
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /universal-token - with auth should succeed",
			Method: http.MethodGet,
			URL:    "/api/argus/universal-token",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{"active", "token", "permanent"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /universal-token - enable permanent should succeed",
			Method: http.MethodGet,
			URL:    "/api/argus/universal-token?enable=1&permanent=1&token=permanent-token-123",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{"\"permanent\":true", "permanent-token-123"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /universal-token - superuser should fail",
			Method: http.MethodGet,
			URL:    "/api/argus/universal-token",
			Headers: map[string]string{
				"Authorization": superuserToken,
			},
			ExpectedStatus:  403,
			ExpectedContent: []string{"Superusers cannot use universal tokens"},
			TestAppFactory: func(t testing.TB) *pbTests.TestApp {
				return hub.TestApp
			},
		},
		{
			Name:   "GET /universal-token - with readonly auth should fail",
			Method: http.MethodGet,
			URL:    "/api/argus/universal-token",
			Headers: map[string]string{
				"Authorization": readOnlyUserToken,
			},
			ExpectedStatus:  403,
			ExpectedContent: []string{"The authorized record is not allowed to perform this action."},
			TestAppFactory:  testAppFactory,
		},
		// /package-updates route
		// /systemd routes

		// Auth Optional Routes - Should work without authentication
		{
			Name:            "GET /getkey - no auth should fail",
			Method:          http.MethodGet,
			URL:             "/api/argus/getkey",
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /getkey - with auth should also succeed",
			Method: http.MethodGet,
			URL:    "/api/argus/getkey",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{"\"key\":", "\"v\":"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /info - should return the same as /getkey",
			Method: http.MethodGet,
			URL:    "/api/argus/info",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{"\"key\":", "\"v\":"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:            "GET /first-run - no auth should succeed",
			Method:          http.MethodGet,
			URL:             "/api/argus/first-run",
			ExpectedStatus:  200,
			ExpectedContent: []string{"\"firstRun\":false"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /first-run - with auth should also succeed",
			Method: http.MethodGet,
			URL:    "/api/argus/first-run",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{"\"firstRun\":false"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:            "GET /agent-connect - no auth should succeed (websocket upgrade fails but route is accessible)",
			Method:          http.MethodGet,
			URL:             "/api/argus/agent-connect",
			ExpectedStatus:  400,
			ExpectedContent: []string{},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /update - shouldn't exist without CHECK_UPDATES env var",
			Method: http.MethodGet,
			URL:    "/api/argus/update",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			NotExpectedContent: []string{"v:", "\"v\":"},
			ExpectedStatus:     404,
			TestAppFactory:     testAppFactory,
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestUniversalTokenEndpoint(t *testing.T) {
	hub, user := argusTests.GetHubWithUser(t)
	defer hub.Cleanup()

	userToken, err := user.NewAuthToken()
	require.NoError(t, err, "Failed to create auth token")

	otherUser, err := argusTests.CreateUser(hub, "other@example.com", "password123")
	require.NoError(t, err, "Failed to create other user")
	otherUserToken, err := otherUser.NewAuthToken()
	require.NoError(t, err, "Failed to create other user auth token")

	readOnlyUser, err := argusTests.CreateUserWithRole(hub, "readonly@example.com", "password123", "readonly")
	require.NoError(t, err, "Failed to create readonly user")
	readOnlyUserToken, err := readOnlyUser.NewAuthToken()
	require.NoError(t, err, "Failed to create readonly user auth token")

	superuser, err := argusTests.CreateSuperuser(hub, "superuser@example.com", "password123")
	require.NoError(t, err, "Failed to create superuser")
	superuserToken, err := superuser.NewAuthToken()
	require.NoError(t, err, "Failed to create superuser auth token")

	testAppFactory := func(t testing.TB) *pbTests.TestApp {
		return hub.TestApp
	}

	// token responses should never be cached
	expectNoStore := func(t testing.TB, app *pbTests.TestApp, res *http.Response) {
		require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	}

	// asserts whether the user has a permanent token record with the given token
	expectPermanentRecord := func(userID, token string, exists bool) func(testing.TB, *pbTests.TestApp, *http.Response) {
		return func(t testing.TB, app *pbTests.TestApp, res *http.Response) {
			expectNoStore(t, app, res)
			rec, err := app.FindFirstRecordByFilter("universal_tokens", "user = {:user}", dbx.Params{"user": userID})
			if !exists {
				require.Error(t, err, "Expected no permanent token record")
				return
			}
			require.NoError(t, err, "Expected permanent token record")
			require.Equal(t, token, rec.GetString("token"))
		}
	}

	const path = "/api/argus/universal-token"
	const token = "post-universal-token-123"

	// scenarios share app state and run in order
	scenarios := []argusTests.ApiScenario{
		{
			Name:            "POST /universal-token - no auth should fail",
			Method:          http.MethodPost,
			URL:             path,
			Body:            jsonReader(map[string]any{"enable": 1}),
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "POST /universal-token - readonly user should fail",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"enable": 1}),
			Headers: map[string]string{
				"Authorization": readOnlyUserToken,
			},
			ExpectedStatus:  403,
			ExpectedContent: []string{"The authorized record is not allowed to perform this action."},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "POST /universal-token - superuser should fail",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"enable": 1}),
			Headers: map[string]string{
				"Authorization": superuserToken,
			},
			ExpectedStatus:  403,
			ExpectedContent: []string{"Superusers cannot use universal tokens"},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectNoStore,
		},
		{
			Name:   "POST /universal-token - invalid json should fail",
			Method: http.MethodPost,
			URL:    path,
			Body:   strings.NewReader(`{`),
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{"Invalid universal token request"},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectNoStore,
		},
		{
			Name:   "POST /universal-token - non-numeric enable should fail",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"enable": "yes"}),
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{"Invalid universal token request"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "POST /universal-token - enable ephemeral token",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"token": token, "enable": 1}),
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"` + token + `"`, `"active":true`, `"permanent":false`},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectPermanentRecord(user.Id, "", false),
		},
		{
			Name:   "POST /universal-token - empty body should read current state",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{}),
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"` + token + `"`, `"active":true`, `"permanent":false`},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectNoStore,
		},
		{
			Name:   "POST /universal-token - make token permanent",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"token": token, "enable": 1, "permanent": 1}),
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"` + token + `"`, `"active":true`, `"permanent":true`},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectPermanentRecord(user.Id, token, true),
		},
		{
			Name:   "POST /universal-token - other user should not read token",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{}),
			Headers: map[string]string{
				"Authorization": otherUserToken,
			},
			ExpectedStatus:     200,
			ExpectedContent:    []string{`"active":false`},
			NotExpectedContent: []string{token},
			TestAppFactory:     testAppFactory,
		},
		{
			Name:   "POST /universal-token - other user should see token as inactive",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"token": token}),
			Headers: map[string]string{
				"Authorization": otherUserToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"` + token + `"`, `"active":false`},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "POST /universal-token - other user should not disable token",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"enable": 0}),
			Headers: map[string]string{
				"Authorization": otherUserToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"active":false`},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectPermanentRecord(user.Id, token, true),
		},
		{
			Name:   "POST /universal-token - disable permanent token",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{"enable": 0}),
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"active":false`, `"permanent":false`},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectPermanentRecord(user.Id, "", false),
		},
		{
			Name:   "POST /universal-token - read after disable should be inactive",
			Method: http.MethodPost,
			URL:    path,
			Body:   jsonReader(map[string]any{}),
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:     200,
			ExpectedContent:    []string{`"active":false`, `"permanent":false`},
			NotExpectedContent: []string{token},
			TestAppFactory:     testAppFactory,
		},
		{
			Name:   "GET /universal-token - legacy query params should still work",
			Method: http.MethodGet,
			URL:    path + "?enable=1&permanent=1&token=legacy-token-123",
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"legacy-token-123"`, `"active":true`, `"permanent":true`},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectPermanentRecord(user.Id, "legacy-token-123", true),
		},
		{
			Name:   "GET /universal-token - legacy read without query params",
			Method: http.MethodGet,
			URL:    path,
			Headers: map[string]string{
				"Authorization": userToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"token":"legacy-token-123"`, `"active":true`, `"permanent":true`},
			TestAppFactory:  testAppFactory,
			AfterTestFunc:   expectNoStore,
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestFirstUserCreation(t *testing.T) {
	t.Run("CreateUserEndpoint available when no users exist", func(t *testing.T) {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		defer hub.Cleanup()

		hub.StartHub()

		testAppFactoryExisting := func(t testing.TB) *pbTests.TestApp {
			return hub.TestApp
		}

		scenarios := []argusTests.ApiScenario{
			{
				Name:   "POST /create-user - should be available when no users exist",
				Method: http.MethodPost,
				URL:    "/api/argus/create-user",
				Body: jsonReader(map[string]any{
					"email":    "firstuser@example.com",
					"password": "password123",
				}),
				ExpectedStatus:  200,
				ExpectedContent: []string{"User created"},
				TestAppFactory:  testAppFactoryExisting,
				BeforeTestFunc: func(t testing.TB, app *pbTests.TestApp, e *core.ServeEvent) {
					userCount, err := hub.CountRecords("users")
					require.NoError(t, err)
					require.Zero(t, userCount, "Should start with no users")
					superusers, err := hub.FindAllRecords(core.CollectionNameSuperusers)
					require.NoError(t, err)
					require.EqualValues(t, 1, len(superusers), "Should start with one temporary superuser")
					require.EqualValues(t, migrations.TempAdminEmail, superusers[0].GetString("email"), "Should have created one temporary superuser")
				},
				AfterTestFunc: func(t testing.TB, app *pbTests.TestApp, res *http.Response) {
					userCount, err := hub.CountRecords("users")
					require.NoError(t, err)
					require.EqualValues(t, 1, userCount, "Should have created one user")
					superusers, err := hub.FindAllRecords(core.CollectionNameSuperusers)
					require.NoError(t, err)
					require.EqualValues(t, 1, len(superusers), "Should have created one superuser")
					require.EqualValues(t, "firstuser@example.com", superusers[0].GetString("email"), "Should have created one superuser")
				},
			},
			{
				Name:   "POST /create-user - should not be available when users exist",
				Method: http.MethodPost,
				URL:    "/api/argus/create-user",
				Body: jsonReader(map[string]any{
					"email":    "firstuser@example.com",
					"password": "password123",
				}),
				ExpectedStatus:  404,
				ExpectedContent: []string{"wasn't found"},
				TestAppFactory:  testAppFactoryExisting,
			},
		}

		for _, scenario := range scenarios {
			scenario.Test(t)
		}
	})

	t.Run("CreateUserEndpoint not available when USER_EMAIL, USER_PASSWORD are set", func(t *testing.T) {
		t.Setenv("ARGUS_HUB_USER_EMAIL", "me@example.com")
		t.Setenv("ARGUS_HUB_USER_PASSWORD", "password123")

		hub, _ := argusTests.NewTestHub(t.TempDir())
		defer hub.Cleanup()

		hub.StartHub()

		testAppFactory := func(t testing.TB) *pbTests.TestApp {
			return hub.TestApp
		}

		scenario := argusTests.ApiScenario{
			Name:            "POST /create-user - should not be available when USER_EMAIL, USER_PASSWORD are set",
			Method:          http.MethodPost,
			URL:             "/api/argus/create-user",
			ExpectedStatus:  404,
			ExpectedContent: []string{"wasn't found"},
			TestAppFactory:  testAppFactory,
			BeforeTestFunc: func(t testing.TB, app *pbTests.TestApp, e *core.ServeEvent) {
				users, err := hub.FindAllRecords("users")
				require.NoError(t, err)
				require.EqualValues(t, 1, len(users), "Should start with one user")
				require.EqualValues(t, "me@example.com", users[0].GetString("email"), "Should have created one user")
				superusers, err := hub.FindAllRecords(core.CollectionNameSuperusers)
				require.NoError(t, err)
				require.EqualValues(t, 1, len(superusers), "Should start with one superuser")
				require.EqualValues(t, "me@example.com", superusers[0].GetString("email"), "Should have created one superuser")
			},
			AfterTestFunc: func(t testing.TB, app *pbTests.TestApp, res *http.Response) {
				users, err := hub.FindAllRecords("users")
				require.NoError(t, err)
				require.EqualValues(t, 1, len(users), "Should still have one user")
				require.EqualValues(t, "me@example.com", users[0].GetString("email"), "Should have created one user")
				superusers, err := hub.FindAllRecords(core.CollectionNameSuperusers)
				require.NoError(t, err)
				require.EqualValues(t, 1, len(superusers), "Should still have one superuser")
				require.EqualValues(t, "me@example.com", superusers[0].GetString("email"), "Should have created one superuser")
			},
		}

		scenario.Test(t)
	})
}

func TestFirstUserBootstrapAtomicity(t *testing.T) {
	t.Run("concurrent complete requests produce exactly one winner", func(t *testing.T) {
		hub, handler := firstUserTestMux(t)
		defer hub.Cleanup()

		start := make(chan struct{})
		statuses := make(chan int, 2)
		for _, email := range []string{"first@example.com", "second@example.com"} {
			go func(email string) {
				<-start
				statuses <- postFirstUser(handler, email).Code
			}(email)
		}
		close(start)

		got := []int{<-statuses, <-statuses}
		sort.Ints(got)
		require.Equal(t, []int{http.StatusOK, http.StatusForbidden}, got)

		users, err := hub.FindAllRecords("users")
		require.NoError(t, err)
		require.Len(t, users, 1)
		superusers, err := hub.FindAllRecords(core.CollectionNameSuperusers)
		require.NoError(t, err)
		require.Len(t, superusers, 1)
		require.NotEqual(t, migrations.TempAdminEmail, superusers[0].Email())
	})

	t.Run("partial body cannot retain stale bootstrap authorization", func(t *testing.T) {
		hub, handler := firstUserTestMux(t)
		defer hub.Cleanup()

		body, err := json.Marshal(map[string]string{
			"email":    "parked@example.com",
			"password": "password123",
		})
		require.NoError(t, err)
		gated := &gatedReader{
			data:    body,
			started: make(chan struct{}),
			release: make(chan struct{}),
		}
		parkedRequest := httptest.NewRequest(http.MethodPost, "/api/argus/create-user", gated)
		parkedRequest.Header.Set("Content-Type", "application/json")
		parkedRecorder := httptest.NewRecorder()
		parkedDone := make(chan struct{})
		go func() {
			handler.ServeHTTP(parkedRecorder, parkedRequest)
			close(parkedDone)
		}()

		select {
		case <-gated.started:
		case <-time.After(2 * time.Second):
			t.Fatal("parked request did not begin reading its body")
		}

		operatorRecorder := postFirstUser(handler, "operator@example.com")
		require.Equal(t, http.StatusOK, operatorRecorder.Code)
		lateRecorder := postFirstUser(handler, "late@example.com")
		require.Equal(t, http.StatusForbidden, lateRecorder.Code)

		close(gated.release)
		select {
		case <-parkedDone:
		case <-time.After(2 * time.Second):
			t.Fatal("parked request did not finish")
		}
		require.Equal(t, http.StatusForbidden, parkedRecorder.Code)

		users, err := hub.FindAllRecords("users")
		require.NoError(t, err)
		require.Len(t, users, 1)
		require.Equal(t, "operator@example.com", users[0].Email())
		superusers, err := hub.FindAllRecords(core.CollectionNameSuperusers)
		require.NoError(t, err)
		require.Len(t, superusers, 1)
		require.Equal(t, "operator@example.com", superusers[0].Email())
	})
}

func TestCreateUserEndpointAvailability(t *testing.T) {
	t.Run("CreateUserEndpoint available when no users exist", func(t *testing.T) {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		defer hub.Cleanup()

		// Ensure no users exist
		userCount, err := hub.CountRecords("users")
		require.NoError(t, err)
		require.Zero(t, userCount, "Should start with no users")

		hub.StartHub()

		testAppFactory := func(t testing.TB) *pbTests.TestApp {
			return hub.TestApp
		}

		scenario := argusTests.ApiScenario{
			Name:   "POST /create-user - should be available when no users exist",
			Method: http.MethodPost,
			URL:    "/api/argus/create-user",
			Body: jsonReader(map[string]any{
				"email":    "firstuser@example.com",
				"password": "password123",
			}),
			ExpectedStatus:  200,
			ExpectedContent: []string{"User created"},
			TestAppFactory:  testAppFactory,
		}

		scenario.Test(t)

		// Verify user was created
		userCount, err = hub.CountRecords("users")
		require.NoError(t, err)
		require.EqualValues(t, 1, userCount, "Should have created one user")
	})

	t.Run("CreateUserEndpoint not available when users exist", func(t *testing.T) {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		defer hub.Cleanup()

		// Create a user first
		_, err := argusTests.CreateUser(hub, "existing@example.com", "password")
		require.NoError(t, err)

		hub.StartHub()

		testAppFactory := func(t testing.TB) *pbTests.TestApp {
			return hub.TestApp
		}

		scenario := argusTests.ApiScenario{
			Name:   "POST /create-user - should not be available when users exist",
			Method: http.MethodPost,
			URL:    "/api/argus/create-user",
			Body: jsonReader(map[string]any{
				"email":    "another@example.com",
				"password": "password123",
			}),
			ExpectedStatus:  404,
			ExpectedContent: []string{"wasn't found"},
			TestAppFactory:  testAppFactory,
		}

		scenario.Test(t)
	})
}

func TestAutoLoginMiddleware(t *testing.T) {
	var hubs []*argusTests.TestHub

	defer func() {
		for _, hub := range hubs {
			hub.Cleanup()
		}
	}()

	t.Setenv("AUTO_LOGIN", "user@test.com")

	testAppFactory := func(t testing.TB) *pbTests.TestApp {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		hubs = append(hubs, hub)
		hub.StartHub()
		return hub.TestApp
	}

	scenarios := []argusTests.ApiScenario{
		{
			Name:            "GET /getkey - without auto login should fail",
			Method:          http.MethodGet,
			URL:             "/api/argus/getkey",
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:            "GET /getkey - with auto login should fail if no matching user",
			Method:          http.MethodGet,
			URL:             "/api/argus/getkey",
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:            "GET /getkey - with auto login should succeed",
			Method:          http.MethodGet,
			URL:             "/api/argus/getkey",
			ExpectedStatus:  200,
			ExpectedContent: []string{"\"key\":", "\"v\":"},
			TestAppFactory:  testAppFactory,
			BeforeTestFunc: func(t testing.TB, app *pbTests.TestApp, e *core.ServeEvent) {
				argusTests.CreateUser(app, "user@test.com", "password123")
			},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestTrustedHeaderMiddleware(t *testing.T) {
	var hubs []*argusTests.TestHub

	defer func() {
		for _, hub := range hubs {
			hub.Cleanup()
		}
	}()

	t.Setenv("TRUSTED_AUTH_HEADER", "X-Argus-Trusted")

	testAppFactory := func(t testing.TB) *pbTests.TestApp {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		hubs = append(hubs, hub)
		hub.StartHub()
		return hub.TestApp
	}

	scenarios := []argusTests.ApiScenario{
		{
			Name:            "GET /getkey - without trusted header should fail",
			Method:          http.MethodGet,
			URL:             "/api/argus/getkey",
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /getkey - with trusted header should fail if no matching user",
			Method: http.MethodGet,
			URL:    "/api/argus/getkey",
			Headers: map[string]string{
				"X-Argus-Trusted": "user@test.com",
			},
			ExpectedStatus:  401,
			ExpectedContent: []string{"requires valid"},
			TestAppFactory:  testAppFactory,
		},
		{
			Name:   "GET /getkey - with trusted header should succeed",
			Method: http.MethodGet,
			URL:    "/api/argus/getkey",
			Headers: map[string]string{
				"X-Argus-Trusted": "user@test.com",
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{"\"key\":", "\"v\":"},
			TestAppFactory:  testAppFactory,
			BeforeTestFunc: func(t testing.TB, app *pbTests.TestApp, e *core.ServeEvent) {
				argusTests.CreateUser(app, "user@test.com", "password123")
			},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestTrustedHeaderProxyAllowlist(t *testing.T) {
	var hubs []*argusTests.TestHub

	defer func() {
		for _, hub := range hubs {
			hub.Cleanup()
		}
	}()

	testAppFactory := func(t testing.TB) *pbTests.TestApp {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		hubs = append(hubs, hub)
		hub.StartHub()
		return hub.TestApp
	}

	// httptest requests arrive from 192.0.2.1:1234
	testCases := []struct {
		name            string
		proxies         string
		expectedStatus  int
		expectedContent []string
	}{
		{
			name:            "peer inside an allowed range",
			proxies:         "10.0.0.0/8, 192.0.2.0/24",
			expectedStatus:  200,
			expectedContent: []string{"\"key\":", "\"v\":"},
		},
		{
			name:            "peer is the listed address",
			proxies:         "192.0.2.1",
			expectedStatus:  200,
			expectedContent: []string{"\"key\":", "\"v\":"},
		},
		{
			name:            "peer outside the allowlist",
			proxies:         "10.0.0.0/8",
			expectedStatus:  401,
			expectedContent: []string{"requires valid"},
		},
		{
			name:            "allowlist with no valid entry",
			proxies:         "proxy.internal",
			expectedStatus:  401,
			expectedContent: []string{"requires valid"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRUSTED_AUTH_HEADER", "X-Argus-Trusted")
			t.Setenv("TRUSTED_PROXY_IPS", tc.proxies)

			scenario := argusTests.ApiScenario{
				Name:   "GET /getkey - with trusted header",
				Method: http.MethodGet,
				URL:    "/api/argus/getkey",
				Headers: map[string]string{
					"X-Argus-Trusted": "user@test.com",
				},
				ExpectedStatus:  tc.expectedStatus,
				ExpectedContent: tc.expectedContent,
				TestAppFactory:  testAppFactory,
				BeforeTestFunc: func(t testing.TB, app *pbTests.TestApp, e *core.ServeEvent) {
					argusTests.CreateUser(app, "user@test.com", "password123")
				},
			}
			scenario.Test(t)
		})
	}
}

func TestTrustedHeaderWithoutProxyWarning(t *testing.T) {
	var hubs []*argusTests.TestHub

	defer func() {
		for _, hub := range hubs {
			hub.Cleanup()
		}
	}()

	testAppFactory := func(t testing.TB) *pbTests.TestApp {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		hubs = append(hubs, hub)
		hub.StartHub()
		return hub.TestApp
	}

	testCases := []struct {
		name          string
		env           map[string]string
		expectWarning []string
		expectNone    []string
	}{
		{
			name:       "header not set",
			expectNone: []string{"TRUSTED_AUTH_HEADER"},
		},
		{
			name:          "trusted header without allowlist",
			env:           map[string]string{"TRUSTED_AUTH_HEADER": "X-Argus-Trusted"},
			expectWarning: []string{"TRUSTED_AUTH_HEADER is set without TRUSTED_PROXY_IPS", "header=X-Argus-Trusted"},
		},
		{
			name:       "trusted header with allowlist",
			env:        map[string]string{"TRUSTED_AUTH_HEADER": "X-Argus-Trusted", "TRUSTED_PROXY_IPS": "10.0.0.5"},
			expectNone: []string{"TRUSTED_AUTH_HEADER"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			scenario := argusTests.ApiScenario{
				Name:            "GET /api/health",
				Method:          http.MethodGet,
				URL:             "/api/health",
				ExpectedStatus:  200,
				ExpectedContent: []string{"API is healthy"},
				TestAppFactory:  testAppFactory,
				AfterTestFunc: func(t testing.TB, app *pbTests.TestApp, res *http.Response) {
					for _, want := range tc.expectWarning {
						require.Contains(t, logs.String(), want)
					}
					for _, unwanted := range tc.expectNone {
						require.NotContains(t, logs.String(), unwanted)
					}
				},
			}
			scenario.Test(t)
		})
	}
}

func TestTrustedHeaderUnknownUserWarning(t *testing.T) {
	var hubs []*argusTests.TestHub

	defer func() {
		for _, hub := range hubs {
			hub.Cleanup()
		}
	}()

	t.Setenv("TRUSTED_AUTH_HEADER", "X-Argus-Trusted")

	testAppFactory := func(t testing.TB) *pbTests.TestApp {
		hub, _ := argusTests.NewTestHub(t.TempDir())
		hubs = append(hubs, hub)
		hub.StartHub()
		return hub.TestApp
	}

	const warning = "No user matches the automatic login email"

	testCases := []struct {
		name            string
		method          string
		url             string
		headerEmail     string
		createUser      bool
		expectedStatus  int
		expectedContent []string
		expectWarning   bool
	}{
		{
			name:            "auth refresh with unknown email",
			method:          http.MethodPost,
			url:             "/api/collections/users/auth-refresh",
			headerEmail:     "unknown@test.com",
			expectedStatus:  401,
			expectedContent: []string{"requires valid"},
			expectWarning:   true,
		},
		{
			name:            "auth refresh with known email",
			method:          http.MethodPost,
			url:             "/api/collections/users/auth-refresh",
			headerEmail:     "user@test.com",
			createUser:      true,
			expectedStatus:  200,
			expectedContent: []string{"\"token\":"},
		},
		{
			name:            "auth refresh without header",
			method:          http.MethodPost,
			url:             "/api/collections/users/auth-refresh",
			expectedStatus:  401,
			expectedContent: []string{"requires valid"},
		},
		{
			name:            "other request with unknown email",
			method:          http.MethodGet,
			url:             "/api/health",
			headerEmail:     "unknown@test.com",
			expectedStatus:  200,
			expectedContent: []string{"API is healthy"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			headers := map[string]string{}
			if tc.headerEmail != "" {
				headers["X-Argus-Trusted"] = tc.headerEmail
			}

			scenario := argusTests.ApiScenario{
				Name:            tc.method + " " + tc.url,
				Method:          tc.method,
				URL:             tc.url,
				Headers:         headers,
				ExpectedStatus:  tc.expectedStatus,
				ExpectedContent: tc.expectedContent,
				TestAppFactory:  testAppFactory,
				BeforeTestFunc: func(t testing.TB, app *pbTests.TestApp, e *core.ServeEvent) {
					if tc.createUser {
						// the users collection only authenticates verified users
						user, err := argusTests.CreateUser(app, tc.headerEmail, "password123")
						require.NoError(t, err)
						user.SetVerified(true)
						require.NoError(t, app.Save(user))
					}
				},
				AfterTestFunc: func(t testing.TB, app *pbTests.TestApp, res *http.Response) {
					if tc.expectWarning {
						require.Contains(t, logs.String(), warning)
						require.Contains(t, logs.String(), "email="+tc.headerEmail)
					} else {
						require.NotContains(t, logs.String(), warning)
					}
				},
			}
			scenario.Test(t)
		})
	}
}

func TestGetInfoConcurrentWithGetSSHKey(t *testing.T) {
	hub, handler := firstUserTestMux(t)
	t.Cleanup(hub.Cleanup)

	user, err := argusTests.CreateUserWithRole(hub, "user@example.com", "password123", "user")
	require.NoError(t, err)
	token, err := user.NewAuthToken()
	require.NoError(t, err)

	dir := t.TempDir()
	codes := make([]int, 8)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			hub.ResetSSHKey()
			_, errs[i] = hub.GetSSHKey(dir)
		}(i)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/argus/info", nil)
			req.Header.Set("Authorization", token)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			codes[i] = recorder.Code
		}(i)
	}
	wg.Wait()
	for i := range codes {
		require.NoError(t, errs[i])
		require.Equal(t, http.StatusOK, codes[i])
	}
}
