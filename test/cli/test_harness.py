"""Regression tests for CI evidence; no running Lightstack or OpenStack CLI needed."""
import copy
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


CLI_ROOT = Path(__file__).resolve().parent
REPO_ROOT = CLI_ROOT.parent.parent


class HarnessTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.complete = []
        for manifest in sorted((CLI_ROOT / "manifest").glob("*.json")):
            for command in json.loads(manifest.read_text())["commands"]:
                for tier in command.get("tiers", ["A"]):
                    if tier in ("A", "B", "C"):
                        cls.complete.append({
                            "id": command["id"] + (".neg" if tier == "B" else ""),
                            "status": "pass",
                        })

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.results = Path(self.directory.name) / "results.json"
        self.baseline = Path(self.directory.name) / "known_failures.txt"
        self.set_baseline()

    def set_baseline(self, *ids):
        lines = [f"{case_id}  # test reason\n" for case_id in ids]
        self.baseline.write_text("".join(lines))

    def gate(self, cases, *flags):
        self.results.write_text(json.dumps({"cases": cases}))
        return self.run_gate(*flags)

    def run_gate(self, *flags):
        env = dict(os.environ, CLI_BASELINE_FILE=str(self.baseline))
        return subprocess.run(
            ["bash", str(CLI_ROOT / "bin/coverage.sh"), "--quiet",
             "--results", str(self.results), *flags],
            capture_output=True, text=True, check=False, env=env,
        )

    def strict_report_failed(self, cases):
        """Run report_failed the way run.sh --strict does (JSONL input)."""
        jsonl = "\n".join(json.dumps(case) for case in cases) + "\n"
        self.results.write_text(jsonl)
        script = (
            'CLI_ROOT="$1"; source "$CLI_ROOT/lib/report.sh"; '
            'log_error() { echo "ERROR: $*" >&2; }; '
            'log_warn() { echo "WARNING: $*" >&2; }; '
            'CLI_STRICT=1 RESULTS_FILE="$2" CLI_BASELINE_FILE="$3" report_failed'
        )
        return subprocess.run(
            ["bash", "-c", script, "report-test", str(CLI_ROOT),
             str(self.results), str(self.baseline)],
            capture_output=True, text=True, check=False,
        )

    def test_complete_results_pass_both_tier_modes(self):
        for flags in ([], ["--strict"]):
            with self.subTest(flags=flags):
                result = self.gate(self.complete, *flags)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_baseline_known_failure_remains_visible_and_allowed(self):
        cases = copy.deepcopy(self.complete)
        cases[0]["status"] = "known-failing"
        self.set_baseline(cases[0]["id"])
        result = self.gate(cases)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("1 known-failing", result.stdout)

    def test_new_known_failure_is_not_allowed(self):
        cases = copy.deepcopy(self.complete)
        cases[0]["status"] = "known-failing"
        result = self.gate(cases)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not in baseline", result.stdout)

    def test_stale_baseline_entry_warns_but_passes(self):
        self.set_baseline(self.complete[0]["id"])
        result = self.gate(self.complete)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("stale known-failure baseline", result.stderr)

    def test_missing_baseline_fails_closed(self):
        self.baseline.unlink()
        self.assertNotEqual(self.gate(self.complete).returncode, 0)

    def test_strict_run_gate_uses_the_same_baseline(self):
        cases = copy.deepcopy(self.complete)
        cases[0]["status"] = "known-failing"

        # A new known-failing case fails the nightly's run gate (report_failed
        # returns 0 = run has a failing case).
        self.assertEqual(self.strict_report_failed(cases).returncode, 0)
        # The same case passes once the ledger explains it.
        self.set_baseline(cases[0]["id"])
        self.assertEqual(self.strict_report_failed(cases).returncode, 1)

    def test_strict_run_gate_warns_when_a_baseline_entry_now_passes(self):
        self.set_baseline(self.complete[0]["id"])
        result = self.strict_report_failed(self.complete)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("stale baseline entry", result.stderr)

    def test_failed_skipped_and_untested_cases_fail(self):
        for status in ("fail", "skip", "untested"):
            with self.subTest(status=status):
                cases = copy.deepcopy(self.complete)
                if status == "untested":
                    cases.pop(0)
                else:
                    cases[0]["status"] = status
                self.assertNotEqual(self.gate(cases).returncode, 0)

    def test_unknown_status_cannot_turn_a_case_green(self):
        for status in ("broken", "", None):
            with self.subTest(status=status):
                cases = copy.deepcopy(self.complete)
                cases[0]["status"] = status
                self.assertNotEqual(self.gate(cases).returncode, 0)

    def test_duplicate_id_cannot_hide_a_failed_retry(self):
        failure = dict(self.complete[0], status="fail")
        for cases in ([*self.complete, failure], [failure, *self.complete]):
            with self.subTest(first_status=cases[0]["status"]):
                self.assertNotEqual(self.gate(cases).returncode, 0)

    def test_missing_empty_and_malformed_reports_fail(self):
        self.assertNotEqual(self.run_gate().returncode, 0)
        for payload in ("", "{", "[]", "{}", '{"cases": []}', '{"cases": {}}'):
            with self.subTest(payload=payload):
                self.results.write_text(payload)
                self.assertNotEqual(self.run_gate().returncode, 0)

    def test_report_revision_is_the_checkout_under_test(self):
        expected = subprocess.check_output(
            ["git", "-C", str(REPO_ROOT), "rev-parse", "--short", "HEAD"], text=True,
        ).strip()
        actual = subprocess.check_output(
            ["bash", "-c", 'CLI_ROOT="$1"; source "$CLI_ROOT/lib/report.sh"; _git_rev',
             "report-test", str(CLI_ROOT)], text=True,
        ).strip()
        self.assertEqual(actual, expected)


if __name__ == "__main__":
    unittest.main()

