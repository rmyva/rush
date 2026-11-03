package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Revert-checks: removing the per-call disk read (a TTL cache) makes the
// second-store revocation assertion pass; using a project-derived path instead
// of GlobalAPITokenStore's global path makes the project-isolation test fail.
func TestGlobalAPITokenStoreIgnoresProjectDirectory(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	_, global := isolateAllGlobalConfigPaths(t)
	t.Setenv(GlobalDirEnv, "")
	project := filepath.Join(t.TempDir(), "project")
	store := GlobalAPITokenStore()
	want := filepath.Join(global, apiTokenFileName)
	if store.Path != want {
		t.Fatalf("global token path = %q, want %q", store.Path, want)
	}
	plain, _, err := store.CreateAPIToken("global", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, apiTokenFileName), []byte(`{"version":1,"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.VerifyAPIToken(plain, time.Now()); err != nil || !ok {
		t.Fatalf("valid global token affected by project file: %v, %v", ok, err)
	}
}

// Revert-check: replacing constant-time hash comparison with an id-only match
// makes the forged-secret control pass. Making VerifyAPIToken cache results
// makes the second-store revocation check accept the revoked token.
func TestAPITokenStoreCreateVerifyAndRevocationAcrossStores(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), "global", apiTokenFileName)
	first, second := &APITokenStore{Path: path}, &APITokenStore{Path: path}
	expires := time.Now().Add(time.Hour)
	plain, record, err := first.CreateAPIToken("ci", []string{"run"}, "ask", &expires)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "rush_"+record.ID+".") {
		t.Fatalf("malformed token: %q", plain)
	}
	if !apiTokenSecretPattern.MatchString(strings.SplitN(plain, ".", 2)[1]) {
		t.Fatalf("secret format invalid: %q", plain)
	}
	if !apiTokenIDPattern.MatchString(record.ID) {
		t.Fatalf("invalid token id: %q", record.ID)
	}
	if strings.Contains(record.Hash, plain) || record.Hash == strings.SplitN(plain, ".", 2)[1] {
		t.Fatal("plaintext token or secret stored as digest")
	}
	if _, ok, err := second.VerifyAPIToken(plain, time.Now()); err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	wrong := plain[:len(plain)-1] + "A"
	if wrong == plain {
		wrong = plain[:len(plain)-1] + "B"
	}
	if _, ok, err := second.VerifyAPIToken(wrong, time.Now()); err == nil || ok {
		t.Fatalf("forged secret accepted: %v, %v", ok, err)
	}
	if err := second.RevokeAPIToken(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := first.VerifyAPIToken(plain, time.Now()); err == nil || ok {
		t.Fatalf("revocation not immediately visible: %v, %v", ok, err)
	}
}

// Revert-check: treating JSON decode errors as an empty store would authorize
// or silently ignore the malformed file instead of failing closed.
// Revert-check: a cache lets one store retain old authorization after another
// store revokes; the malformed-store test also catches non-fail-closed reads.
func TestAPITokenStoreExpiryMalformedFailClosedAndProjectPathIgnored(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	root := t.TempDir()
	global := filepath.Join(root, "global", apiTokenFileName)
	project := filepath.Join(root, "project", apiTokenFileName)
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"version":1,"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &APITokenStore{Path: global}
	past := time.Now().Add(-time.Second)
	plain, record, err := store.CreateAPIToken("expired", []string{"read"}, "ask", &past)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.VerifyAPIToken(plain, time.Now()); ok || err != ErrInvalidAPIToken {
		t.Fatalf("expired credential accepted: %v, %v", ok, err)
	}
	if _, err := os.Stat(global); err != nil {
		t.Fatal(err)
	}
	store.Path = project
	if _, ok, err := store.VerifyAPIToken(plain, time.Now()); ok || err != ErrInvalidAPIToken {
		t.Fatalf("project record unexpectedly authorized: %v, %v", ok, err)
	}
	store.Path = global
	if err := os.WriteFile(global, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.VerifyAPIToken(plain, time.Now()); err == nil || ok {
		t.Fatalf("malformed store did not fail closed: %v, %v (id %s)", ok, err, record.ID)
	}
}

// This simultaneous-start test is probabilistic; TestAPITokenConcurrentCreateLockIsDeterministic
// exercises the lock ordering with an explicit barrier.
func TestAPITokenStoreConcurrentCreateAndPermissions(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), "tokens.json")
	stores := []*APITokenStore{{Path: path}, {Path: path}}
	const count = 8
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		go func(i int) {
			_, _, err := stores[i%len(stores)].CreateAPIToken("worker", []string{"read"}, "ask", nil)
			errs <- err
		}(i)
	}
	for i := 0; i < count; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	items, err := stores[0].ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != count {
		t.Fatalf("got %d tokens, want %d", len(items), count)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permission bits are not portable to Windows")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token store mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAPITokenHashIsNeverSerializedOrPrinted(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
	_, record, err := store.CreateAPIToken("safe", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), record.Hash) || strings.Contains(record.String(), record.Hash) {
		t.Fatalf("digest escaped API record: %s / %s", encoded, record.String())
	}
	if _, _, err := store.CreateAPIToken("unsafe", []string{"run"}, "auto", nil); err == nil {
		t.Fatal("auto permission without admin scope accepted")
	}
}

func TestConsumeServeBootstrapTokenScopeAndUnset(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
	plain := "rush_01020304." + strings.Repeat("A", 43)
	value := "01020304:" + APITokenHash(plain)
	env := map[string]string{"RUSH_SERVE_TOKEN_SHA256": value}
	unset := func(key string) error { delete(env, key); return nil }
	got, ok, err := ConsumeServeBootstrapToken(store, func(key string) string { return env[key] }, unset, time.Now())
	if err != nil || !ok || len(got.Scopes) != 1 || got.Scopes[0] != "run" {
		t.Fatalf("bootstrap: %#v %v %v", got, ok, err)
	}
	verified, ok, err := store.VerifyAPIToken(plain, time.Now())
	if err != nil || !ok || verified.ID != got.ID {
		t.Fatalf("memory-only bootstrap verification: %#v %v %v", verified, ok, err)
	}
	if _, ok := env["RUSH_SERVE_TOKEN_SHA256"]; ok {
		t.Fatal("bootstrap digest remains in environment")
	}
}

func TestConsumeServeBootstrapTokenRejectsAdminScope(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
	plain := "rush_01020304." + strings.Repeat("A", 43)
	value := "01020304:" + APITokenHash(plain)
	env := map[string]string{"RUSH_SERVE_TOKEN_SHA256": value, "RUSH_SERVE_TOKEN_SCOPE": "admin"}
	_, ok, err := ConsumeServeBootstrapToken(store, func(key string) string { return env[key] }, func(key string) error { delete(env, key); return nil }, time.Now())
	if err == nil || ok {
		t.Fatalf("admin bootstrap scope accepted: ok=%v err=%v", ok, err)
	}
}

func TestConsumeServeBootstrapTokenUnsetBeforeValidation(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
	value := "bad"
	unset := false
	_, _, err := ConsumeServeBootstrapToken(store, func(string) string { return value }, func(string) error { unset = true; value = ""; return nil }, time.Now())
	if err == nil || !unset {
		t.Fatalf("invalid bootstrap should fail after unsetting: err=%v unset=%v", err, unset)
	}
}

func TestAPITokenScopeAllowsMatrix(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	scopes := []string{"read", "run", "config", "admin"}
	for hi, have := range scopes {
		for ni, need := range scopes {
			if got, want := APITokenScopeAllows([]string{have}, need), hi >= ni; got != want {
				t.Errorf("have %q need %q = %v, want %v", have, need, got, want)
			}
		}
	}
	for _, have := range [][]string{nil, {"unknown"}} {
		for _, need := range []string{"read", "unknown"} {
			if APITokenScopeAllows(have, need) {
				t.Errorf("unexpected grant have=%v need=%q", have, need)
			}
		}
	}
	if !APITokenScopeAllows([]string{"admin", "unknown"}, "read") || APITokenScopeAllows([]string{"admin"}, "unknown") {
		t.Fatal("unknown scope handling is incorrect")
	}
}

func TestAPITokenTouchIsMemoryOnlyAndFlushMerges(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := &APITokenStore{Path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	originalTokenFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	originalClock := apiTokenClock
	defer func() { apiTokenClock = originalClock }()
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	apiTokenClock = func() time.Time { return now }
	store.Touch("01020304")
	store.Touch("01020304")
	if got, err := os.ReadFile(path); err != nil || string(got) != string(originalTokenFile) {
		t.Fatalf("Touch changed token file: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "api-tokens.state")); !os.IsNotExist(err) {
		t.Fatalf("Touch wrote state file: %v", err)
	}
	store.RecordAPITokenUse("01020304", now.Add(time.Hour))
	store.RecordAPITokenUse("01020304", now.Add(-time.Hour))
	store.RecordAPITokenUse("11111111", now.Add(-time.Hour))
	state := filepath.Join(filepath.Dir(path), "api-tokens.state")
	if err := os.WriteFile(state, []byte(`{"22222222":"2025-01-02T03:04:05Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]time.Time
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got["01020304"].Equal(now.Add(time.Hour)) || !got["22222222"].Equal(now) || !got["11111111"].Equal(now.Add(-time.Hour)) {
		t.Fatalf("merged state = %#v", got)
	}
	flushedTokenFile, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(flushedTokenFile) != string(originalTokenFile) {
		t.Fatal("Flush changed token file")
	}
	before := string(data)
	store.Touch("33333333")
	now = now.Add(30 * time.Second)
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(state)
	if string(after) != before {
		t.Fatal("flush was not throttled for one minute")
	}
}

func TestAPITokenFlushMalformedStateNeverOverwrites(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := &APITokenStore{Path: path}
	state := filepath.Join(filepath.Dir(path), "api-tokens.state")
	bad := []byte(`{broken`)
	if err := os.WriteFile(state, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	store.Touch("01020304")
	if err := store.Flush(); err == nil {
		t.Fatal("malformed state accepted")
	}
	got, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bad) {
		t.Fatalf("malformed state overwritten: %q", got)
	}
}

func TestConsumeServeBootstrapTokenOnceAndRealEnvironment(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	t.Setenv(GlobalDirEnv, "")
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
	calls := 0
	_, _, firstErr := ConsumeServeBootstrapToken(store, func(string) string { calls++; return "bad" }, func(string) error { return nil }, time.Now())
	_, _, secondErr := ConsumeServeBootstrapToken(store, func(string) string { calls++; return "" }, func(string) error { return nil }, time.Now())
	if firstErr == nil || secondErr == nil || calls != 2 {
		t.Fatalf("not memoized result: calls=%d errors=%v/%v", calls, firstErr, secondErr)
	}

	plain := "rush_01020304." + strings.Repeat("A", 43)
	t.Setenv("RUSH_SERVE_TOKEN_SHA256", "01020304:"+APITokenHash(plain))
	t.Setenv("RUSH_SERVE_TOKEN_SCOPE", "read")
	envStore := &APITokenStore{Path: filepath.Join(t.TempDir(), "env-tokens.json")}
	got, ok, err := ConsumeServeBootstrapTokenFromEnv(envStore, time.Now())
	if err != nil || !ok || got.ID != "01020304" {
		t.Fatalf("real environment consume: %#v %v %v", got, ok, err)
	}
	if _, ok := os.LookupEnv("RUSH_SERVE_TOKEN_SHA256"); ok {
		t.Fatal("digest environment variable remains")
	}
	if _, ok := os.LookupEnv("RUSH_SERVE_TOKEN_SCOPE"); ok {
		t.Fatal("scope environment variable remains")
	}
	t.Setenv("RUSH_SERVE_TOKEN_SHA256", "invalid-second-value")
	t.Setenv("RUSH_SERVE_TOKEN_SCOPE", "second-sentinel")
	second, secondOK, secondErr := ConsumeServeBootstrapToken(envStore, func(string) string { t.Fatal("environment reread"); return "" }, func(string) error { t.Fatal("environment unset repeated"); return nil }, time.Now())
	if secondErr != nil || !secondOK || second.ID != got.ID {
		t.Fatalf("second consume changed first result: %#v %v %v", second, secondOK, secondErr)
	}
	second.Scopes[0] = "admin"
	if second.Expires != nil {
		*second.Expires = time.Time{}
	}
	third, thirdOK, thirdErr := ConsumeServeBootstrapToken(envStore, nil, nil, time.Now())
	if thirdErr != nil || !thirdOK || third.ID != got.ID || len(third.Scopes) != 1 || third.Scopes[0] != "read" {
		t.Fatalf("mutation of memoized result escaped: %#v %v %v", third, thirdOK, thirdErr)
	}
	if got := os.Getenv("RUSH_SERVE_TOKEN_SHA256"); got != "invalid-second-value" {
		t.Fatalf("second consume changed digest environment variable: %q", got)
	}
	if got := os.Getenv("RUSH_SERVE_TOKEN_SCOPE"); got != "second-sentinel" {
		t.Fatalf("second consume changed scope environment variable: %q", got)
	}
}

func TestAPITokenReadCacheUsesStatBeforeReadAndRefreshes(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	first, second := &APITokenStore{Path: path}, &APITokenStore{Path: path}
	plain, record, err := first.CreateAPIToken("cache", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := first.VerifyAPIToken(plain, time.Now()); err != nil || !ok {
		t.Fatalf("initial verify: %v %v", ok, err)
	}
	reads := 0
	first.readHook = func() { reads++ }
	if _, ok, err := first.VerifyAPIToken(plain, time.Now()); err != nil || !ok {
		t.Fatalf("unchanged verify: %v %v", ok, err)
	}
	if reads != 0 {
		t.Fatalf("unchanged read performed %d reads", reads)
	}
	if err := second.RevokeAPIToken(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := first.VerifyAPIToken(plain, time.Now()); err == nil || ok {
		t.Fatalf("revoked token accepted: %v %v", ok, err)
	}
	if reads != 1 {
		t.Fatalf("changed file reads = %d, want 1", reads)
	}
}

func TestAPITokenVerifyUniformErrorsAndCompare(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	expires := time.Now().Add(time.Hour)
	plain, record, err := store.CreateAPIToken("uniform", []string{"read"}, "ask", &expires)
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte(plain)
	bad[len(bad)-1] = 'A'
	if string(bad) == plain {
		bad[len(bad)-1] = 'B'
	}
	_, _, badErr := store.VerifyAPIToken(string(bad), time.Now())
	_, _, expiredErr := store.VerifyAPIToken(plain, expires.Add(time.Second))
	if expiredErr != ErrInvalidAPIToken {
		t.Fatalf("expired error = %v, want uniform invalid-token error", expiredErr)
	}
	if err := store.RevokeAPIToken(record.ID); err != nil {
		t.Fatal(err)
	}
	_, _, revokedErr := store.VerifyAPIToken(plain, time.Now())
	_, _, unknownErr := store.VerifyAPIToken("rush_ffffffff."+strings.Repeat("A", 43), time.Now())
	if badErr == nil || revokedErr == nil || expiredErr == nil || unknownErr == nil || badErr.Error() != revokedErr.Error() || badErr.Error() != expiredErr.Error() || badErr.Error() != unknownErr.Error() {
		t.Fatalf("nonuniform errors: %v / %v / %v / %v", badErr, revokedErr, expiredErr, unknownErr)
	}
	calls := 0
	oldHook := apiTokenCompareHook
	apiTokenCompareHook = func(a, b []byte) int { calls++; return 0 }
	defer func() { apiTokenCompareHook = oldHook }()
	_, _, _ = store.VerifyAPIToken("rush_ffffffff."+strings.Repeat("A", 43), time.Now())
	if calls != 1 {
		t.Fatalf("unknown compare calls = %d", calls)
	}
	calls = 0
	_, _, _ = store.VerifyAPIToken(plain, time.Now())
	if calls != 1 {
		t.Fatalf("known compare calls = %d", calls)
	}
}

func TestAPITokenParserExactShapeAndCopiedDigest(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	plain, record, err := store.CreateAPIToken("shape", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := []string{plain}
	for _, token := range valid {
		if _, ok, err := store.VerifyAPIToken(token, time.Now()); err != nil || !ok {
			t.Fatalf("valid token: %v %v", ok, err)
		}
	}
	parts := strings.SplitN(plain, ".", 2)
	invalid := []string{"", "R" + plain[1:], "rush_ABCDEF12." + parts[1], "rush_" + parts[0][len("rush_"):] + "." + parts[1][:42], "rush_" + parts[0][len("rush_"):] + "." + parts[1] + "A", "rush_" + parts[0][len("rush_"):] + "." + parts[1] + "=", plain + ".x", plain + " ", " " + plain}
	for _, token := range invalid {
		if _, ok, err := store.VerifyAPIToken(token, time.Now()); err == nil || ok {
			t.Errorf("accepted malformed token %q", token)
		}
	}
	secondPlain, _, err := store.CreateAPIToken("other", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	var target *APIToken
	for i := range items {
		if items[i].ID == strings.SplitN(secondPlain, ".", 2)[0][len("rush_"):] {
			target = &items[i]
			break
		}
	}
	if target == nil {
		t.Fatal("second token record not found")
	}
	target.Hash = record.Hash
	writePath, err := store.selectedPath()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeSettingsDocument(mustTokenJSON(t, items), settingsDocumentFormatOf(writePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.VerifyAPIToken(secondPlain, time.Now()); err == nil || ok {
		t.Fatal("digest copied across ids verified")
	}
}

func TestAPITokenParserDirectTable(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	valid := "rush_0123abcd." + strings.Repeat("A", 43)
	cases := []struct {
		name  string
		plain string
		want  bool
	}{
		{"valid", valid, true},
		{"missing prefix", strings.TrimPrefix(valid, "rush_"), false},
		{"uppercase id", "rush_0123ABCD." + strings.Repeat("A", 43), false},
		{"seven character id", "rush_123abcd." + strings.Repeat("A", 43), false},
		{"nine character id", "rush_0123abcde." + strings.Repeat("A", 43), false},
		{"non-hex id", "rush_0123abcz." + strings.Repeat("A", 43), false},
		{"42 character secret", "rush_0123abcd." + strings.Repeat("A", 42), false},
		{"44 character secret", "rush_0123abcd." + strings.Repeat("A", 44), false},
		{"padding", valid + "=", false},
		{"outside base64url", "rush_0123abcd." + strings.Repeat("A", 42) + "+", false},
		{"extra dot", valid + ".x", false},
		{"leading whitespace", " " + valid, false},
		{"trailing whitespace", valid + " ", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := parseAPIToken(tc.plain)
			if ok != tc.want || (ok && id != "0123abcd") {
				t.Fatalf("parseAPIToken(%q) = %q, %v; want id 0123abcd, %v", tc.plain, id, ok, tc.want)
			}
		})
	}
}

// Mutant check: returning a cached APIToken without clone() lets callers alter cached scopes and timestamps.
func TestAPITokenReturnedRecordsAreDeepCopies(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	expires := time.Now().Add(time.Hour)
	plain, created, err := store.CreateAPIToken("clone", []string{"read", "run"}, "ask", &expires)
	if err != nil {
		t.Fatal(err)
	}
	show, err := store.ShowAPIToken(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := store.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	verified, ok, err := store.VerifyAPIToken(plain, time.Now())
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
	for _, record := range []*APIToken{&created, &show, &list[0], &verified} {
		record.Scopes[0] = "admin"
		*record.Expires = time.Time{}
	}
	later, ok, err := store.VerifyAPIToken(plain, time.Now())
	if err != nil || !ok || later.Scopes[0] != "read" || later.Expires == nil || !later.Expires.Equal(expires) {
		t.Fatalf("caller mutation affected Verify: %#v %v %v", later, ok, err)
	}
	shown, err := store.ShowAPIToken(created.ID)
	if err != nil || shown.Scopes[0] != "read" || shown.Expires == nil || !shown.Expires.Equal(expires) {
		t.Fatalf("caller mutation affected Show: %#v %v", shown, err)
	}
	if err := store.RevokeAPIToken(created.ID); err != nil {
		t.Fatal(err)
	}
	revokedShow, err := store.ShowAPIToken(created.ID)
	if err != nil || revokedShow.RevokedAt == nil {
		t.Fatalf("revoked record missing timestamp: %#v %v", revokedShow, err)
	}
	revokedList, err := store.ListAPITokens()
	if err != nil || revokedList[0].RevokedAt == nil {
		t.Fatalf("revoked list record missing timestamp: %#v %v", revokedList, err)
	}
	*revokedShow.RevokedAt = time.Time{}
	*revokedList[0].RevokedAt = time.Time{}
	revokedAgain, err := store.ShowAPIToken(created.ID)
	if err != nil || revokedAgain.RevokedAt == nil || revokedAgain.RevokedAt.IsZero() {
		t.Fatalf("caller mutation affected RevokedAt: %#v %v", revokedAgain, err)
	}
}

// Mutant check: a bootstrap result or Verify result must not alias the cached bootstrap record.
func TestAPITokenBootstrapRecordsAreDeepCopies(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	plain := "rush_01020304." + strings.Repeat("A", 43)
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), apiTokenFileName)}
	values := map[string]string{"RUSH_SERVE_TOKEN_SHA256": "01020304:" + APITokenHash(plain)}
	first, ok, err := ConsumeServeBootstrapToken(store, func(k string) string { return values[k] }, func(string) error { return nil }, time.Now())
	if err != nil || !ok {
		t.Fatalf("bootstrap = %v, %v", ok, err)
	}
	verified, ok, err := store.VerifyAPIToken(plain, time.Now())
	if err != nil || !ok {
		t.Fatalf("Verify bootstrap = %v, %v", ok, err)
	}
	first.Scopes[0] = "admin"
	verified.Scopes[0] = "admin"
	repeated, ok, err := ConsumeServeBootstrapToken(store, nil, nil, time.Now())
	if err != nil || !ok || repeated.Scopes[0] != "run" {
		t.Fatalf("bootstrap cache mutated: %#v %v %v", repeated, ok, err)
	}
	again, ok, err := store.VerifyAPIToken(plain, time.Now())
	if err != nil || !ok || again.Scopes[0] != "run" {
		t.Fatalf("bootstrap verification mutated: %#v %v %v", again, ok, err)
	}
}

// apiTokenLockContentionObservationWindow remains wall-clock based because the
// available hooks bracket lock acquisition but cannot signal that B is blocked inside it.
const apiTokenLockContentionObservationWindow = 300 * time.Millisecond

// Mutant check: removing the cross-store file lock lets B complete while A is held in its mutation hook.
func TestAPITokenConcurrentCreateLockIsDeterministic(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	t.Setenv(GlobalDirEnv, "")
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	storeA, storeB := &APITokenStore{Path: path}, &APITokenStore{Path: path}
	entered, release := make(chan struct{}), make(chan struct{})
	hookTimeout := make(chan struct{}, 1)
	storeA.mutateHook = func() {
		close(entered)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			hookTimeout <- struct{}{}
		}
	}
	aboutToLockB := make(chan struct{}, 1)
	oldBeforeLockHook := apiTokenBeforeLockHook
	apiTokenBeforeLockHook = func(store *APITokenStore) {
		if store == storeB {
			aboutToLockB <- struct{}{}
		}
	}
	t.Cleanup(func() { apiTokenBeforeLockHook = oldBeforeLockHook })
	type result struct {
		plain string
		err   error
	}
	aDone, bDone := make(chan result, 1), make(chan result, 1)
	go func() {
		plain, _, err := storeA.CreateAPIToken("A", []string{"read"}, "ask", nil)
		aDone <- result{plain, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("A did not reach mutation hook")
	}
	go func() {
		plain, _, err := storeB.CreateAPIToken("B", []string{"read"}, "ask", nil)
		bDone <- result{plain, err}
	}()
	select {
	case <-aboutToLockB:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("B did not reach before-lock hook")
	}
	select {
	case got := <-bDone:
		close(release)
		t.Fatalf("B completed while A held lock: %v", got.err)
	case <-time.After(apiTokenLockContentionObservationWindow):
	}
	close(release)
	var ar, br result
	select {
	case ar = <-aDone:
	case <-time.After(10 * time.Second):
		t.Fatal("A did not finish after lock release")
	}
	select {
	case br = <-bDone:
	case <-time.After(10 * time.Second):
		t.Fatal("B did not finish after lock release")
	}
	select {
	case <-hookTimeout:
		t.Fatal("A mutation hook timed out waiting for lock release")
	default:
	}
	if ar.err != nil || br.err != nil {
		t.Fatalf("create errors: %v / %v", ar.err, br.err)
	}
	items, err := storeA.ListAPITokens()
	if err != nil || len(items) != 2 {
		t.Fatalf("records = %d, %v", len(items), err)
	}
	for _, plain := range []string{ar.plain, br.plain} {
		if _, ok, err := storeB.VerifyAPIToken(plain, time.Now()); err != nil || !ok {
			t.Fatalf("verify = %v, %v", ok, err)
		}
	}
}

// Mutant check: state merging must be monotonic and flushing is throttled at 60 seconds.
func TestAPITokenFlushMonotonicAndExactThrottle(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	if err := os.WriteFile(path, []byte(`{"version":1,"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(filepath.Dir(path), "api-tokens.state")
	newer := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	older := newer.Add(-time.Hour)
	if err := os.WriteFile(state, []byte(`{"01020304":"2025-02-03T04:05:06Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store.RecordAPITokenUse("01020304", older)
	originalClock := apiTokenClock
	now := newer.Add(time.Hour)
	apiTokenClock = func() time.Time { return now }
	t.Cleanup(func() { apiTokenClock = originalClock })
	tokenBefore, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	bytesAfter, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]time.Time
	if err := json.Unmarshal(bytesAfter, &got); err != nil {
		t.Fatal(err)
	}
	if !got["01020304"].Equal(newer) {
		t.Fatalf("older timestamp overwrote newer: %v", got)
	}
	if tokenAfter, err := os.ReadFile(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if string(tokenAfter) != string(tokenBefore) {
		t.Fatal("Flush changed token file")
	}
	store.RecordAPITokenUse("11111111", now)
	stateBefore := string(bytesAfter)
	now = now.Add(59 * time.Second)
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	stateAfter, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(stateAfter) != stateBefore {
		t.Fatal("flush wrote before 60 seconds")
	}
	now = now.Add(time.Second)
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	stateAfter, err = os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(stateAfter) == stateBefore {
		t.Fatal("flush did not write at 60 seconds")
	}
	if tokenAfter, err := os.ReadFile(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if string(tokenAfter) != string(tokenBefore) {
		t.Fatal("Flush changed token file")
	}
}

func TestErrAPITokenNotFoundSentinel(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	store := &APITokenStore{Path: filepath.Join(t.TempDir(), apiTokenFileName)}
	if _, err := store.ShowAPIToken("01020304"); !errors.Is(err, ErrAPITokenNotFound) {
		t.Fatalf("Show error = %v", err)
	}
	if err := store.RevokeAPIToken("01020304"); !errors.Is(err, ErrAPITokenNotFound) {
		t.Fatalf("Revoke error = %v", err)
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", ErrAPITokenNotFound), ErrAPITokenNotFound) {
		t.Fatal("wrapped sentinel not recognized")
	}
}

// Mutant requires changing the shared apiTokenDigest helper used by creation and verification to hash only the secret.
func TestAPITokenHashBindsIDWhenSecretIsShared(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	secret := strings.Repeat("A", 43)
	tokenA, tokenB := "rush_01020304."+secret, "rush_05060708."+secret
	records := []APIToken{
		{ID: "01020304", Name: "A", Scopes: []string{"read"}, Permission: "ask", Created: time.Now().UTC(), Hash: APITokenHash(tokenA)},
		{ID: "05060708", Name: "B", Scopes: []string{"read"}, Permission: "ask", Created: time.Now().UTC(), Hash: APITokenHash(tokenB)},
	}
	if err := os.WriteFile(path, mustTokenJSON(t, records), 0o600); err != nil {
		t.Fatal(err)
	}
	store.cacheMu.Lock()
	store.cacheValid = false
	store.cacheMu.Unlock()
	if _, ok, err := store.VerifyAPIToken(tokenA, time.Now()); err != nil || !ok {
		t.Fatalf("initial token A verification: %v, %v", ok, err)
	}
	// Record A's digest is copied onto B's id; under a secret-only hash, B's
	// same secret would then verify, so the assertion detects that mutation.
	records[1].Hash = records[0].Hash
	if err := os.WriteFile(path, mustTokenJSON(t, records), 0o600); err != nil {
		t.Fatal(err)
	}
	store.cacheMu.Lock()
	store.cacheValid = false
	store.cacheMu.Unlock()
	if _, ok, err := store.VerifyAPIToken(tokenB, time.Now()); ok || err != ErrInvalidAPIToken {
		t.Fatalf("copied digest verified token B: %v, %v", ok, err)
	}
}

func mustTokenJSON(t *testing.T, tokens []APIToken) []byte {
	t.Helper()
	data, err := json.Marshal(apiTokenDocument{Version: apiTokenStoreVersion, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAPITokenKtavRoundTripAndMalformedCreate(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	for _, ktav := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "ktav"}[ktav], func(t *testing.T) {
			restore := SetKtavAutoMigrateForTest(ktav)
			defer restore()
			path := filepath.Join(t.TempDir(), apiTokenFileName)
			store := &APITokenStore{Path: path}
			plain, _, err := store.CreateAPIToken("roundtrip", []string{"read"}, "ask", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := (&APITokenStore{Path: path}).VerifyAPIToken(plain, time.Now()); err != nil || !ok {
				t.Fatalf("roundtrip: %v %v", ok, err)
			}
			path, err = store.selectedPath()
			if err != nil {
				t.Fatal(err)
			}
			if ktav && !strings.HasSuffix(path, ".ktav") {
				t.Fatal("expected Ktav path")
			}
		})
	}
	restore := SetKtavAutoMigrateForTest(true)
	defer restore()
	path := filepath.Join(t.TempDir(), apiTokenKtavFileName)
	if err := os.WriteFile(path, []byte("not valid = ="), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&APITokenStore{Path: path}).ListAPITokens(); err == nil {
		t.Fatal("malformed Ktav accepted")
	}
	untouched, _ := os.ReadFile(path)
	if _, _, err := (&APITokenStore{Path: path}).CreateAPIToken("cannot convert", []string{"read"}, "ask", nil); err == nil {
		t.Fatal("unrepresentable write accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(untouched) {
		t.Fatal("failed conversion changed file")
	}
}

func TestAPITokenKtavCreateRejectsUnrepresentableRecord(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	restore := SetKtavAutoMigrateForTest(true)
	defer restore()
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	if _, _, err := store.CreateAPIToken("line\rbreak", []string{"read"}, "ask", nil); err == nil {
		t.Fatal("Ktav create accepted a record containing an unrepresentable CR")
	}
	selected, err := store.selectedPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(selected); !os.IsNotExist(err) {
		t.Fatalf("failed Ktav create left a file: %v", err)
	}
}

func TestAPITokenStateUnreadableFailsFlush(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not enforced consistently on Windows")
	}
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	store := &APITokenStore{Path: path}
	state := filepath.Join(filepath.Dir(path), "api-tokens.state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store.Touch("01020304")
	if err := store.Flush(); err == nil {
		t.Fatal("unreadable state flush succeeded")
	}
}

func TestAPITokenVerifyMalformedVersionFailsClosed(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), apiTokenFileName)
	if err := os.WriteFile(path, []byte(`{"version":2,"tokens":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &APITokenStore{Path: path}
	if _, ok, err := store.VerifyAPIToken("rush_01020304."+strings.Repeat("A", 43), time.Now()); err == nil || ok {
		t.Fatal("wrong version did not fail closed")
	}
	if _, err := store.ListAPITokens(); err == nil {
		t.Fatal("wrong version listed")
	}
}

func TestConsumeServeBootstrapTokenRejectsCollisionAndBadDigestLength(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := &APITokenStore{Path: path}
	if _, _, err := store.CreateAPIToken("collision", []string{"read"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	id := items[0].ID
	for _, digest := range []string{strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		tmp := &APITokenStore{Path: filepath.Join(t.TempDir(), "tokens.json")}
		_, ok, err := ConsumeServeBootstrapToken(tmp, func(k string) string {
			if k == "RUSH_SERVE_TOKEN_SHA256" {
				return id + ":" + digest
			}
			return ""
		}, func(string) error { return nil }, time.Now())
		if err == nil || ok {
			t.Fatalf("bad digest length accepted: %d", len(digest))
		}
	}
	collision := &APITokenStore{Path: path}
	_, ok, err := ConsumeServeBootstrapToken(collision, func(k string) string {
		if k == "RUSH_SERVE_TOKEN_SHA256" {
			return id + ":" + strings.Repeat("a", 64)
		}
		return ""
	}, func(string) error { return nil }, time.Now())
	if err == nil || ok {
		t.Fatal("stored id collision accepted")
	}
}
