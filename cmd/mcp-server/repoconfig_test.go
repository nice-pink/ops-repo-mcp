package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepoConfig creates a temp dir acting as an ops repo root and writes
// .ops-repo-mcp.yaml into it with the given content.
func writeRepoConfig(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(repoConfigPath(root), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return root
}

func TestLoadRepoConfigAbsentIsNotAnError(t *testing.T) {
	rc, err := loadRepoConfig(t.TempDir())
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if rc != nil {
		t.Fatalf("expected nil config for missing file, got %+v", rc)
	}
}

func TestLoadRepoConfigEmptyFileIsNotAnError(t *testing.T) {
	root := writeRepoConfig(t, "")
	rc, err := loadRepoConfig(root)
	if err != nil {
		t.Fatalf("expected no error for empty file, got %v", err)
	}
	if rc == nil {
		t.Fatal("expected non-nil config for present-but-empty file")
	}
	if rc.Layout.Base != "" {
		t.Fatalf("expected zero layout, got %+v", rc.Layout)
	}
}

func TestLoadRepoConfigHappyPath(t *testing.T) {
	root := writeRepoConfig(t, `
version: 1
layout:
  base: base/apps
  namespace: streaming
  pathScheme: "{base}/{app}/{env}"
  imageFileName: deployment.yaml
  imageHistoryFileName: history.txt
  srcEnv: staging
`)
	rc, err := loadRepoConfig(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Layout.Base != "base/apps" {
		t.Errorf("base = %q", rc.Layout.Base)
	}
	if rc.Layout.PathScheme != "{base}/{app}/{env}" {
		t.Errorf("pathScheme = %q", rc.Layout.PathScheme)
	}
	if rc.Layout.SrcEnv != "staging" {
		t.Errorf("srcEnv = %q", rc.Layout.SrcEnv)
	}
	if rc.Version != repoConfigVersion {
		t.Errorf("version = %d", rc.Version)
	}
}

func TestLoadRepoConfigOmittedVersionIsAccepted(t *testing.T) {
	root := writeRepoConfig(t, "layout:\n  base: base/apps\n")
	rc, err := loadRepoConfig(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Version != repoConfigVersion {
		t.Errorf("expected version normalised to %d, got %d", repoConfigVersion, rc.Version)
	}
}

func TestLoadRepoConfigRejectsUnknownVersion(t *testing.T) {
	root := writeRepoConfig(t, "version: 2\nlayout:\n  base: base/apps\n")
	_, err := loadRepoConfig(root)
	if err == nil {
		t.Fatal("expected an error for version 2")
	}
	if !strings.Contains(err.Error(), "version 2 is not supported") {
		t.Errorf("error should name the version, got: %v", err)
	}
}

// The security boundary: a file inside the ops repo must not be able to grant
// the server authority or redirect its credentials. Strict decoding is what
// enforces it, so these keys must fail loudly rather than be ignored.
func TestLoadRepoConfigRejectsOperationalSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"envAllowlist", "version: 1\nenvAllowlist: dev,staging,prod\n"},
		{"nested envAllowlist", "version: 1\nlayout:\n  envAllowlist: prod\n"},
		{"opsRepoPath", "version: 1\nopsRepoPath: /etc\n"},
		{"gitToken", "version: 1\ngitToken: hunter2\n"},
		{"gitSshKeyPath", "version: 1\nlayout:\n  gitSshKeyPath: /root/.ssh/id_rsa\n"},
		{"runnerTimeout", "version: 1\nrunnerTimeout: 9999s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeRepoConfig(t, tc.content)
			_, err := loadRepoConfig(root)
			if err == nil {
				t.Fatalf("expected %s to be rejected, but the file loaded", tc.name)
			}
			if !strings.Contains(err.Error(), "operational settings") {
				t.Errorf("error should explain that operational settings are env-only, got: %v", err)
			}
		})
	}
}

func TestResolveExceptionalAppsFileRejectsUnsafePaths(t *testing.T) {
	for _, tc := range []struct {
		name, rel, wantIn string
	}{
		{"absolute", "/etc/passwd", "must be relative"},
		{"traversal", "../../etc/passwd", "must not contain '..'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &repoConfig{Layout: repoConfigLayout{ExceptionalAppsFile: tc.rel}}
			_, err := rc.resolveExceptionalAppsFile(t.TempDir())
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("want error containing %q, got: %v", tc.wantIn, err)
			}
		})
	}
}

// A directory passing as the exceptions file used to reach the runner's reader,
// which panics on a read failure on a code path with no recover.
func TestResolveExceptionalAppsFileRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "notafile"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := &repoConfig{Layout: repoConfigLayout{ExceptionalAppsFile: "notafile"}}
	_, err := rc.resolveExceptionalAppsFile(root)
	if err == nil {
		t.Fatal("expected a directory to be rejected")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("want 'not a regular file', got: %v", err)
	}
}

// Containment must survive the repo root itself being a symlink. On macOS
// t.TempDir() is already under /var -> /private/var, but on Linux it is not, so
// build the symlink explicitly or this regression only shows up on one OS.
func TestResolveExceptionalAppsFileWithSymlinkedRepoRoot(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "exceptions.yaml"), []byte("apps: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rc := &repoConfig{Layout: repoConfigLayout{ExceptionalAppsFile: "exceptions.yaml"}}
	got, err := rc.resolveExceptionalAppsFile(link)
	if err != nil {
		t.Fatalf("a symlinked repo root must still resolve: %v", err)
	}
	wantRoot, _ := filepath.EvalSymlinks(real)
	if got != filepath.Join(wantRoot, "exceptions.yaml") {
		t.Errorf("got %q, want %q", got, filepath.Join(wantRoot, "exceptions.yaml"))
	}
}

// Only the first YAML document is decoded, so a second one — unknown keys and
// all — would be silently dropped. That contradicts the fail-loudly contract.
func TestLoadRepoConfigRejectsMultipleDocuments(t *testing.T) {
	root := writeRepoConfig(t, "layout:\n  base: first\n---\nlayout:\n  base: second\n  envAllowlist: prod\n")
	_, err := loadRepoConfig(root)
	if err == nil {
		t.Fatal("expected a multi-document file to be rejected")
	}
	if !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("want 'more than one YAML document', got: %v", err)
	}
}

// The layout-only hint belongs on an unknown-key error, not on a syntax error.
func TestUnknownFieldHintOnlyOnUnknownField(t *testing.T) {
	root := writeRepoConfig(t, "version: 1\nenvAllowlist: prod\n")
	_, err := loadRepoConfig(root)
	if err == nil || !strings.Contains(err.Error(), "operational settings") {
		t.Fatalf("unknown key should carry the layout-only hint, got: %v", err)
	}

	root2 := writeRepoConfig(t, "layout: [broken\n")
	_, err2 := loadRepoConfig(root2)
	if err2 == nil {
		t.Fatal("expected a syntax error")
	}
	if strings.Contains(err2.Error(), "operational settings") {
		t.Errorf("a syntax error should not lecture about credentials: %v", err2)
	}
}

func TestLoadRepoConfigRejectsMalformedYAML(t *testing.T) {
	root := writeRepoConfig(t, "layout: [this is not a mapping\n")
	if _, err := loadRepoConfig(root); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}

func TestResolveExceptionalAppsFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "exceptions.yaml"), []byte("apps: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rc := &repoConfig{Layout: repoConfigLayout{ExceptionalAppsFile: "exceptions.yaml"}}
	got, err := rc.resolveExceptionalAppsFile(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// t.TempDir can sit under a symlink (/var -> /private/var on macOS), so
	// compare against the canonical root rather than the raw one.
	wantRoot, _ := filepath.EvalSymlinks(root)
	if got != filepath.Join(wantRoot, "exceptions.yaml") {
		t.Errorf("got %q, want %q", got, filepath.Join(wantRoot, "exceptions.yaml"))
	}
}

func TestResolveExceptionalAppsFileMissingIsAnError(t *testing.T) {
	rc := &repoConfig{Layout: repoConfigLayout{ExceptionalAppsFile: "nope.yaml"}}
	_, err := rc.resolveExceptionalAppsFile(t.TempDir())
	if err == nil {
		t.Fatal("expected an error for a file that does not exist")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should say the file is missing, got: %v", err)
	}
}

func TestResolveExceptionalAppsFileNilAndEmpty(t *testing.T) {
	var nilRC *repoConfig
	if got, err := nilRC.resolveExceptionalAppsFile(t.TempDir()); got != "" || err != nil {
		t.Errorf("nil config: got (%q, %v), want (\"\", nil)", got, err)
	}
	empty := &repoConfig{}
	if got, err := empty.resolveExceptionalAppsFile(t.TempDir()); got != "" || err != nil {
		t.Errorf("empty config: got (%q, %v), want (\"\", nil)", got, err)
	}
}

// resolveExceptionalAppsFile must reject a symlink inside the repo that points
// outside it — the containment check has to survive symlink resolution, not
// just textual '..' filtering.
func TestResolveExceptionalAppsFileRejectsEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.yaml")
	if err := os.WriteFile(secret, []byte("apps: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(root, "link.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rc := &repoConfig{Layout: repoConfigLayout{ExceptionalAppsFile: "link.yaml"}}
	if _, err := rc.resolveExceptionalAppsFile(root); err == nil {
		t.Fatal("expected a symlink escaping the repo root to be rejected")
	}
}

func TestPickLayoutPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		env, file, def   string
		wantVal, wantSrc string
	}{
		{"env wins over file and default", "from-env", "from-file", "from-default", "from-env", srcFromEnv},
		{"file wins over default", "", "from-file", "from-default", "from-file", srcFromFile},
		{"default when neither is set", "", "", "from-default", "from-default", srcFromDefault},
		{"empty env does not shadow the file", "", "from-file", "", "from-file", srcFromFile},
		{"all empty yields empty default", "", "", "", "", srcFromDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			val, src := pickLayout(tc.env, tc.file, tc.def)
			if val != tc.wantVal || src != tc.wantSrc {
				t.Errorf("got (%q, %q), want (%q, %q)", val, src, tc.wantVal, tc.wantSrc)
			}
		})
	}
}

// fakeEnv builds a getenv func over a map, so precedence can be driven without
// mutating the process environment.
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var fullFileLayout = repoConfigLayout{
	Base:                 "file/base",
	Namespace:            "filens",
	PathScheme:           "{base}/{app}/{env}",
	ImageFileName:        "file-deployment.yaml",
	ImageHistoryFileName: "file-history.txt",
	SrcEnv:               "filesrc",
}

// This is the test that catches a mis-wired call site. Each case sets exactly
// one env var and asserts that ONLY the matching field changes source — so
// crossing two fields (env var X feeding field Y) fails here.
func TestResolveLayoutWiring(t *testing.T) {
	fields := []struct {
		field  string
		envVar string
		envVal string
		get    func(layoutValues) string
	}{
		{"base", "DS_BASE", "env/base", func(l layoutValues) string { return l.Base }},
		{"namespace", "DS_NAMESPACE", "envns", func(l layoutValues) string { return l.Namespace }},
		{"pathScheme", "DS_PATH_SCHEME", "{app}/{env}", func(l layoutValues) string { return l.PathScheme }},
		{"imageFileName", "DS_IMAGE_FILE_NAME", "env-deployment.yaml", func(l layoutValues) string { return l.ImageFileName }},
		{"imageHistoryFileName", "DS_IMAGE_HISTORY_FILE_NAME", "env-history.txt", func(l layoutValues) string { return l.ImageHistoryFileName }},
		{"srcEnv", "DS_SRC_ENV", "envsrc", func(l layoutValues) string { return l.SrcEnv }},
	}

	for _, f := range fields {
		t.Run(f.field, func(t *testing.T) {
			lv, src, err := resolveLayout(fakeEnv(map[string]string{f.envVar: f.envVal}), fullFileLayout)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := f.get(lv); got != f.envVal {
				t.Errorf("%s: got %q, want the env value %q — call site may be reading the wrong env var", f.field, got, f.envVal)
			}
			if src[f.field] != srcFromEnv {
				t.Errorf("%s: source = %q, want %q", f.field, src[f.field], srcFromEnv)
			}
			// Every other field must still come from the file.
			for _, other := range fields {
				if other.field == f.field {
					continue
				}
				if src[other.field] != srcFromFile {
					t.Errorf("setting %s changed %s's source to %q — fields are crossed", f.envVar, other.field, src[other.field])
				}
			}
		})
	}
}

func TestResolveLayoutFileSuppliesEverything(t *testing.T) {
	lv, src, err := resolveLayout(fakeEnv(nil), fullFileLayout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lv.Base != "file/base" || lv.Namespace != "filens" || lv.ImageFileName != "file-deployment.yaml" ||
		lv.ImageHistoryFileName != "file-history.txt" || lv.SrcEnv != "filesrc" || lv.PathScheme != "{base}/{app}/{env}" {
		t.Fatalf("file values not applied: %+v", lv)
	}
	for _, f := range []string{"base", "namespace", "pathScheme", "imageFileName", "imageHistoryFileName", "srcEnv"} {
		if src[f] != srcFromFile {
			t.Errorf("%s: source = %q, want %q", f, src[f], srcFromFile)
		}
	}
}

// The plugin's shipped .mcp.json passes DS_* through as empty strings. If empty
// counted as "set", the file would be permanently shadowed and the whole feature
// would be dead on arrival.
func TestResolveLayoutEmptyEnvDoesNotShadowFile(t *testing.T) {
	empty := map[string]string{
		"DS_BASE": "", "DS_NAMESPACE": "", "DS_PATH_SCHEME": "",
		"DS_IMAGE_FILE_NAME": "", "DS_IMAGE_HISTORY_FILE_NAME": "", "DS_SRC_ENV": "",
	}
	lv, src, err := resolveLayout(fakeEnv(empty), fullFileLayout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lv.Base != "file/base" {
		t.Errorf("empty DS_BASE shadowed the file: base = %q", lv.Base)
	}
	if src["base"] != srcFromFile {
		t.Errorf("base source = %q, want %q", src["base"], srcFromFile)
	}
}

func TestResolveLayoutDefaults(t *testing.T) {
	lv, src, err := resolveLayout(fakeEnv(nil), repoConfigLayout{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lv.PathScheme != "{base}/{namespace}/{app}/{env}" {
		t.Errorf("pathScheme default = %q", lv.PathScheme)
	}
	if lv.ImageFileName != "deployment.yaml" {
		t.Errorf("imageFileName default = %q", lv.ImageFileName)
	}
	if lv.SrcEnv != "staging" {
		t.Errorf("srcEnv default = %q", lv.SrcEnv)
	}
	for _, f := range []string{"base", "namespace", "pathScheme", "imageFileName", "imageHistoryFileName", "srcEnv"} {
		if src[f] != srcFromDefault {
			t.Errorf("%s: source = %q, want %q", f, src[f], srcFromDefault)
		}
	}
}

// Validation applies to whichever value wins, and the message must name the
// source the operator actually wrote.
func TestResolveLayoutValidatesResolvedValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    map[string]string
		file   repoConfigLayout
		wantIn string
	}{
		{"file base traversal", nil, repoConfigLayout{Base: "../../etc"}, ".ops-repo-mcp.yaml layout.base"},
		{"file base absolute", nil, repoConfigLayout{Base: "/etc"}, "must be relative"},
		{"env base traversal", map[string]string{"DS_BASE": "../.."}, repoConfigLayout{}, "DS_BASE"},
		{"file base bad chars", nil, repoConfigLayout{Base: "has spaces"}, ".ops-repo-mcp.yaml layout.base"},
		{"file namespace", nil, repoConfigLayout{Namespace: "Bad_Ns"}, ".ops-repo-mcp.yaml layout.namespace"},
		{"env namespace", map[string]string{"DS_NAMESPACE": "Bad_Ns"}, repoConfigLayout{}, "DS_NAMESPACE"},
		{"file pathScheme no app", nil, repoConfigLayout{PathScheme: "{base}/{env}"}, "must contain {app}"},
		{"file pathScheme no env", nil, repoConfigLayout{PathScheme: "{base}/{app}"}, "must contain {env}"},
		{"file pathScheme traversal", nil, repoConfigLayout{PathScheme: "{base}/../{app}/{env}"}, "must not contain '..'"},
		{"env pathScheme", map[string]string{"DS_PATH_SCHEME": "{base}/{env}"}, repoConfigLayout{}, "DS_PATH_SCHEME"},
		{"file imageFileName slash", nil, repoConfigLayout{ImageFileName: "a/b.yaml"}, ".ops-repo-mcp.yaml layout.imageFileName"},
		{"file historyFileName slash", nil, repoConfigLayout{ImageHistoryFileName: "a/b.txt"}, "layout.imageHistoryFileName"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := resolveLayout(fakeEnv(tc.env), tc.file)
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("want error naming %q, got: %v", tc.wantIn, err)
			}
		})
	}
}

// A file value the operator has overridden via its env var must not be able to
// stop the server: repo content should not break a config that does not use it.
func TestResolveLayoutOverriddenBadFileValueIsIgnored(t *testing.T) {
	lv, src, err := resolveLayout(
		fakeEnv(map[string]string{"DS_NAMESPACE": "good", "DS_BASE": "good/base"}),
		repoConfigLayout{Namespace: "Bad_Ns", Base: "../../etc"},
	)
	if err != nil {
		t.Fatalf("an overridden invalid file value must not fail startup, got: %v", err)
	}
	if lv.Namespace != "good" || lv.Base != "good/base" {
		t.Errorf("env values not applied: %+v", lv)
	}
	if src["namespace"] != srcFromEnv || src["base"] != srcFromEnv {
		t.Errorf("sources = %v, want env for both", src)
	}
}

// An invalid srcEnv must NOT be fatal: it previously only affected promote calls
// that omitted srcEnv, and a startup exit would take deploy and rollback down too.
func TestResolveLayoutInvalidSrcEnvIsNotFatal(t *testing.T) {
	lv, _, err := resolveLayout(fakeEnv(map[string]string{"DS_SRC_ENV": "Staging"}), repoConfigLayout{})
	if err != nil {
		t.Fatalf("invalid srcEnv must not be a startup error, got: %v", err)
	}
	if lv.SrcEnv != "Staging" {
		t.Errorf("srcEnv = %q, want the value passed through for per-call validation", lv.SrcEnv)
	}
}

func TestLayoutSourceStringIsStable(t *testing.T) {
	ls := layoutSource{"base": srcFromEnv, "srcEnv": srcFromFile, "namespace": srcFromDefault}
	want := "base=env namespace=default srcEnv=repo-file"
	for i := 0; i < 20; i++ {
		if got := ls.String(); got != want {
			t.Fatalf("iteration %d: got %q, want %q (map order must be sorted)", i, got, want)
		}
	}
}

// A bad committed file must not be able to brick the server with no way out.
func TestLoadRepoConfigIgnoreEscapeHatch(t *testing.T) {
	root := writeRepoConfig(t, "version: 99\nenvAllowlist: prod\n")
	if _, err := loadRepoConfig(root); err == nil {
		t.Fatal("sanity: this file should be rejected without the escape hatch")
	}

	t.Setenv("MCP_IGNORE_REPO_CONFIG", "1")
	rc, err := loadRepoConfig(root)
	if err != nil {
		t.Fatalf("escape hatch should skip the file entirely, got: %v", err)
	}
	if rc != nil {
		t.Errorf("expected the file to be ignored, got %+v", rc)
	}
}

func TestLoadRepoConfigRejectsSymlinkOutOfRepo(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("layout:\n  base: sneaky\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(target, repoConfigPath(root)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := loadRepoConfig(root)
	if err == nil {
		t.Fatal("expected a config symlinked out of the repo to be refused")
	}
	if !strings.Contains(err.Error(), "outside the ops repo") {
		t.Errorf("want 'outside the ops repo', got: %v", err)
	}
}
