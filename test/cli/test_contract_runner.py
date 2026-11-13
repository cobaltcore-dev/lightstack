"""The contract wrapper must not hide failures without individual test results."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


RUNNER = Path(__file__).resolve().parents[1] / "run_contract_tests.sh"


class ContractRunnerTests(unittest.TestCase):
    def test_package_failure_and_empty_suite_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for command in ("curl", "openstack"):
                path = root / command
                path.write_text("#!/bin/sh\nexit 0\n")
                path.chmod(0o755)
            go = root / "go"
            env = dict(os.environ, PATH=f"{root}{os.pathsep}{os.environ['PATH']}")
            for rc, records, expected in (
                (0, [{"Action": "pass", "Test": "TestAPI"}], 0),
                (1, [{"Action": "fail", "Package": "contract"}], 1),
                (0, [{"Action": "skip", "Test": "TestAPI"}], 1),
                (1, [{"Action": "pass", "Test": "TestAPI"}, {"Action": "fail"}], 1),
            ):
                with self.subTest(rc=rc, records=records):
                    lines = "\n".join(json.dumps(record) for record in records)
                    go.write_text(f"#!/bin/sh\ncat <<'EOF'\n{lines}\nEOF\nexit {rc}\n")
                    go.chmod(0o755)
                    result = subprocess.run(["bash", str(RUNNER)], env=env,
                                            text=True, capture_output=True, check=False)
                    self.assertEqual(result.returncode, expected, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
