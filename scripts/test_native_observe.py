"""Exercise the native checker with a fake CLI and isolated fixture configuration."""

import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest


class NativeCheckTest(unittest.TestCase):
    def check_fake(self, changes_fixture_config=False, wrong_answer=False, remote_pool=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture_home = root / "fixture-home"
            fixture_home.mkdir()
            fixture_config = fixture_home / "config.toml"
            fixture_config.write_text('model_provider = "openai"\n')
            pool_config = root / "pool.json"
            pool_config.write_text(json.dumps({"state_dir": "state"}))
            fake = root / "fake-codex"
            fake.write_text(
                "#!" + sys.executable + "\n"
                "import os, pathlib, sys\n"
                "assert os.environ.get('PYTHONUNBUFFERED') == " + repr(os.environ.get("PYTHONUNBUFFERED")) + "\n"
                "assert sys.argv[1:5] == ['exec', '--json', '--ephemeral', '--skip-git-repo-check']\n"
                "assert len(sys.argv) == 8\n"
                "assert sys.argv[5] == '--output-last-message'\n"
                "pathlib.Path(sys.argv[6]).write_text(" + repr("unexpected" if wrong_answer else "NATIVE_RELAY_CHECK_OK") + ")\n"
                + ("(pathlib.Path(os.environ['CODEX_HOME']) / 'config.toml').write_text('# simulated concurrent edit\\n')\n"
                   if changes_fixture_config else "")
            )
            fake.chmod(0o700)
            # This environment belongs only to the fake executable; the installed
            # Codex and its files are never invoked or read by these tests.
            env = dict(os.environ, CODEX_HOME=str(fixture_home))
            command = [sys.executable, str(Path(__file__).with_name("check-native-observe.py")),
                       "pool" if remote_pool else "direct", "--config", str(pool_config), "--codex", str(fake)]
            if remote_pool:
                ca = root / "state/ca"
                ca.mkdir(parents=True)
                (ca / "mitmproxy-ca-cert.pem").touch()
                fake_bridge = root / "fake-mitmdump"
                fake_bridge.write_text(
                    "#!" + sys.executable + "\n"
                    "import os, sys, time\n"
                    "assert os.environ['PYTHONUNBUFFERED'] == '1'\n"
                    "assert 'pool_origin=http://192.0.2.10:18473' in sys.argv\n"
                    "assert 'pool_private_http=true' in sys.argv\n"
                    "print('Local redirector started.', flush=True)\n"
                    "print('pool bridge: pool POST request', flush=True)\n"
                    "time.sleep(30)\n"
                )
                fake_bridge.chmod(0o700)
                command += ["--mitmdump", str(fake_bridge), "--origin", "http://192.0.2.10:18473", "--private-http"]
            result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=20)
            reports = list((root / "state/checks").glob("*/result.json"))
            self.assertEqual(len(reports), 1, result.stderr)
            report = json.loads(reports[0].read_text())
            self.assertEqual(report["bridge_required"], remote_pool)
            self.assertEqual(report["bridge_observed_native_request"], remote_pool)
            self.assertEqual(report["hashes_unchanged"], not changes_fixture_config)
            self.assertEqual(report["expected_response"], not wrong_answer)
            self.assertEqual(report["exit_code"], 0)
            self.assertEqual(result.returncode, int(changes_fixture_config or wrong_answer), result.stderr)
            for name in ("answer.txt", "events.jsonl", "stderr.log", "bridge.log", "result.json"):
                self.assertEqual(stat.S_IMODE((reports[0].parent / name).stat().st_mode), 0o600)
            expected = '# simulated concurrent edit\n' if changes_fixture_config else 'model_provider = "openai"\n'
            self.assertEqual(fixture_config.read_text(), expected)

    def test_direct_success_without_claiming_capture(self):
        self.check_fake()

    def test_config_hash_change_fails_without_restoring_file(self):
        self.check_fake(changes_fixture_config=True)

    def test_wrong_response_fails(self):
        self.check_fake(wrong_answer=True)

    def test_remote_origin_and_private_http_go_only_to_bridge(self):
        self.check_fake(remote_pool=True)


if __name__ == "__main__":
    unittest.main()
