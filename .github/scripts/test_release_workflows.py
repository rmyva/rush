from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]


class ReleaseWorkflowRegressionTests(unittest.TestCase):
    def test_build_commit_comes_from_checked_out_tree(self):
        workflow = (ROOT / ".github/workflows/publish-fork-npm.yml").read_text()
        build = workflow.split("- name: Build binaries and assemble npm packages", 1)[1]
        build = build.split("- name: Verify binary reports the expected version", 1)[0]
        self.assertIn('git rev-parse HEAD', build)
        self.assertNotIn('GITHUB_SHA', build)

    def test_publish_loop_skips_existing_packages_and_fails_closed(self):
        workflow = (ROOT / ".github/workflows/publish-fork-npm.yml").read_text()
        publish = workflow.split('- name: Publish to npm', 1)[1]
        self.assertIn('for d in staging/rush-* staging/rush; do', publish)
        self.assertIn('PACKAGE="${PACKAGE_NAME}@${VERSION}"', publish)
        self.assertIn('npm view "$PACKAGE" version --json', publish)
        self.assertIn('Skipping already-published ${PACKAGE}', publish)
        self.assertIn('continue', publish)
        self.assertIn('npm publish "./$d" $FLAGS', publish)
        self.assertIn('DRY_RUN: ${{ github.event_name != \'push\' }}', publish)
        self.assertIn('Dry-run packaging already-published ${PACKAGE}', publish)
        self.assertIn('FLAGS="$FLAGS --dry-run"', publish)
        self.assertRegex(publish, r'E404\|404 Not Found\|is not in this registry')
        self.assertIn('not a not-found response', publish)
        self.assertNotIn('already exists in the npm registry', publish)

    def test_prerelease_with_build_metadata_does_not_use_latest(self):
        workflow = (ROOT / ".github/workflows/publish-fork-npm.yml").read_text()
        publish = workflow.split('- name: Publish to npm', 1)[1]
        self.assertIn('([.+]|$)', publish)
        match = re.match(r"^[0-9]+\.[0-9]+\.[0-9]+-([0-9A-Za-z-]+)([.+]|$)", "1.2.3-1+foo")
        self.assertIsNotNone(match)
        self.assertEqual(match.group(1), "1")
        self.assertIn('PUBLISH_TAG="${BASH_REMATCH[1]}"', publish)
        self.assertIn('PUBLISH_TAG="prerelease"', publish)

    def test_release_output_uses_injected_version_for_guard(self):
        version = (ROOT / "internal/version/version.go").read_text()
        test = (ROOT / "internal/version/version_test.go").read_text()
        workflow = (ROOT / ".github/workflows/publish-fork-npm.yml").read_text()
        self.assertIn('return "release " + releaseVersion', version)
        self.assertIn('injectedReleaseVersion("v0.2.0-alpha.1")', test)
        self.assertIn('[ "$previous" = "release" ] && [ "$tok" = "$EXPECTED" ]', workflow)
        self.assertIn('EXPECTED="${VERSION}"', workflow)
        self.assertNotIn('forkBaseVersion plus a', workflow)

    def test_license_cache_includes_go_import_graph_after_setup_go(self):
        workflow = (ROOT / ".github/workflows/licenses.yml").read_text()
        setup = workflow.index('uses: actions/setup-go@')
        graph = workflow.index('Go import graph fingerprint')
        fingerprint = workflow.index('name: dependency fingerprint')
        restore = workflow.index('name: already verified since the last run?')
        scan = workflow.index('go-licenses" report ./...')
        self.assertLess(setup, graph)
        self.assertLess(graph, fingerprint)
        self.assertLess(fingerprint, restore)
        self.assertLess(restore, scan)
        self.assertIn('go list -deps', workflow)
        self.assertIn('GOOS="$goos"', workflow)
        self.assertIn('license-ok-v2-', workflow)


if __name__ == "__main__":
    unittest.main()
