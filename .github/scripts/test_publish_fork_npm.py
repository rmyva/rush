"""Regression checks for the fork npm publish workflow wiring."""

import pathlib
import re
import unittest

WORKFLOW = pathlib.Path(__file__).parents[1] / "workflows" / "publish-fork-npm.yml"


class PublishWorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = WORKFLOW.read_text(encoding="utf-8")

    def test_checkout_uses_the_validated_tag_output(self):
        validation = self.workflow.index("      - name: Validate requested release tag")
        emitted_tag = self.workflow.index('echo "tag=$TAG" >> "$GITHUB_OUTPUT"', validation)
        checkout = self.workflow.index("ref: ${{ steps.ver.outputs.tag }}")
        self.assertLess(validation, emitted_tag)
        self.assertLess(emitted_tag, checkout)
        self.assertIn('if [ "$EVENT_NAME" = "workflow_dispatch" ]; then\n            TAG="$INPUT_TAG"', self.workflow[validation:emitted_tag])
        self.assertIn('TAG="$REF_NAME"', self.workflow[validation:emitted_tag])
        self.assertIn(
            "grep -Eq '^npm-v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?(\\+[0-9A-Za-z.-]+)?$'",
            self.workflow[validation:emitted_tag],
        )

    def test_numeric_prerelease_uses_non_latest_channel(self):
        publish_start = self.workflow.index("# A prerelease is anything after the first hyphen")
        publish_end = self.workflow.index('echo "Publishing under dist-tag:', publish_start)
        logic = self.workflow[publish_start:publish_end]
        self.assertIn('if [[ "$VERSION" =~ ^[0-9]+\\.[0-9]+\\.[0-9]+-([0-9A-Za-z-]+)([.+]|$) ]]; then', logic)
        self.assertIn('if [[ "$PUBLISH_TAG" =~ ^[0-9]+$ ]]; then', logic)
        self.assertIn('PUBLISH_TAG="prerelease"', logic)
        self.assertIn('PUBLISH_TAG="latest"', logic)
        self.assertLess(logic.index('PUBLISH_TAG="prerelease"'), logic.index('PUBLISH_TAG="latest"'))

    def test_build_id_is_injected_from_the_tag_commit(self):
        build_start = self.workflow.index('BUILD_COMMIT="$(git rev-parse HEAD)"')
        build_end = self.workflow.index("# target triples:", build_start)
        build = self.workflow[build_start:build_end]
        self.assertRegex(build, r'BUILD_COMMIT="\$\(git rev-parse HEAD\)"')
        self.assertIn('BUILD_COMMIT="${BUILD_COMMIT:0:8}"', build)
        self.assertRegex(build, r'-X github\.com/PHPCraftdream/rush/internal/version\.Commit=\$\{BUILD_COMMIT\}')
        self.assertRegex(build, r'-X github\.com/PHPCraftdream/rush/internal/version\.BuildID=\$\{BUILD_COMMIT\}')
        self.assertEqual(
            re.findall(r'-X github\.com/PHPCraftdream/rush/internal/version\.(?:Commit|BuildID)=\$\{([^}]+)\}', build),
            ["BUILD_COMMIT", "BUILD_COMMIT"],
        )


if __name__ == "__main__":
    unittest.main()
