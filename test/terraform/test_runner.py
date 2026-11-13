"""Exercise the real wrapper with fake clients to verify its evidence gates."""
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


RUNNER = Path(__file__).resolve().parent / "run.sh"
LISTS = ("server list", "network list", "security group list",
         "volume list", "keypair list", "subnet list", "router list")

# Only the external clients are replaced. The wrapper still runs its full
# preflight, workspace handling, apply/destroy, and clean-state assertions.
CLIENT = r'''
import json
import os
import sys
from pathlib import Path

tool = Path(sys.argv[0]).name
args = sys.argv[1:]
if tool == "curl":
    sys.exit(0)
if tool == "openstack":
    command = " ".join(args[:3] if args[0] in ("security", "floating") else args[:2])
    if command == os.environ.get("FAIL_LIST"):
        print("simulated API failure", file=sys.stderr)
        sys.exit(1)
    if command == "catalog list":
        print("http://localhost:35357/v3")
    elif command == "floating ip list":
        print("fip-owned" if os.environ.get("LEAK_LIST") == command else "fip-unrelated")
    elif command.endswith(" list"):
        print("tf-compat-leftover" if os.environ.get("LEAK_LIST") == command else "unrelated")
    else:
        print("fixture-id")
    sys.exit(0)

workspace = None
if args[0].startswith("-chdir="):
    workspace = Path(args.pop(0).split("=", 1)[1])
if args[0] == "version":
    print("Terraform v1.13.4")
elif args[:2] == ["state", "pull"]:
    if os.environ.get("STATE_MODE") == "error":
        sys.exit(1)
    if os.environ.get("STATE_MODE") == "malformed":
        print("{")
    elif os.environ.get("STATE_MODE") == "no-fips":
        print(json.dumps({"version": 4, "resources": []}))
    else:
        print(json.dumps({"version": 4, "resources": [{
            "type": "openstack_networking_floatingip_v2",
            "instances": [{"attributes": {"id": "fip-owned"}}],
        }]}))
elif args[0] == "output":
    print("{}")
elif args[:2] == ["state", "list"]:
    if os.environ.get("FAIL_DESTROY"):
        print("openstack_compute_instance_v2.partial")
elif args[0] == "destroy":
    if os.environ.get("FAIL_DESTROY"):
        (workspace / "terraform.tfstate").write_text("recovery-state")
        sys.exit(1)
    (workspace / "destroyed").touch()
elif args[0] == "plan" and (workspace / "destroyed").exists():
    print(os.environ.get("POST_DESTROY_PLAN", "Plan: 9 to add, 0 to change, 0 to destroy."))
    sys.exit(2)
'''


class TerraformRunnerTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        for tool in ("terraform", "openstack", "curl"):
            script = self.root / tool
            script.write_text(f"#!{sys.executable}\n" + CLIENT)
            script.chmod(0o755)

    def run_wrapper(self, **settings):
        env = dict(os.environ, PATH=f"{self.root}{os.pathsep}{os.environ['PATH']}",
                   OPENSTACK_CLI=str(self.root / "openstack"),
                   TF_PLUGIN_CACHE_DIR=str(self.root / "cache"),
                   TF_RESULTS_DIR=str(self.root / "results"),
                   TMPDIR=str(self.root), FAIL_LIST="", LEAK_LIST="", STATE_MODE="")
        env.update(settings)
        return subprocess.run(["bash", str(RUNNER)], env=env, text=True,
                              capture_output=True, timeout=30, check=False)

    def assert_failed(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("SUMMARY: PASS", result.stdout)

    def test_success_requires_clean_state_and_unrelated_objects_are_ignored(self):
        for mode in ("", "no-fips"):
            with self.subTest(mode=mode):
                result = self.run_wrapper(STATE_MODE=mode)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("SUMMARY: PASS", result.stdout)
                self.assertFalse(list(self.root.glob("tf-compat-workspace.*")))

    def test_failed_resource_listing_cannot_prove_no_residue(self):
        for command in (*LISTS, "floating ip list"):
            with self.subTest(command=command):
                self.assert_failed(self.run_wrapper(FAIL_LIST=command))

    def test_leaked_resources_fail_including_nameless_floating_ips(self):
        for command in (*LISTS, "floating ip list"):
            with self.subTest(command=command):
                self.assert_failed(self.run_wrapper(LEAK_LIST=command))

    def test_unreadable_state_cannot_hide_floating_ips(self):
        for mode in ("error", "malformed"):
            with self.subTest(mode=mode):
                self.assert_failed(self.run_wrapper(STATE_MODE=mode))

    def test_failed_destroy_preserves_recovery_state(self):
        result = self.run_wrapper(FAIL_DESTROY="1")
        self.assert_failed(result)
        workspaces = list(self.root.glob("tf-compat-workspace.*"))
        self.assertEqual(len(workspaces), 1)
        self.assertEqual((workspaces[0] / "terraform.tfstate").read_text(), "recovery-state")
        self.assertIn("retained Terraform workspace", result.stderr)

    def test_post_destroy_plan_requires_positive_evidence(self):
        for plan in ("", "unrecognized output", "Plan: 1 to add, 1 to change, 0 to destroy."):
            with self.subTest(plan=plan):
                self.assert_failed(self.run_wrapper(POST_DESTROY_PLAN=plan))

    def test_catalog_failure_cannot_pass_preflight(self):
        self.assert_failed(self.run_wrapper(FAIL_LIST="catalog list"))


if __name__ == "__main__":
    unittest.main()
