"""install-local.sh tests; HOME is a temp dir and no service is installed."""

import json
import os
import platform
import plistlib
import re
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest


REPO = Path(__file__).resolve().parent.parent
SCRIPT = REPO / "scripts" / "install-local.sh"
SYSTEM_PATH = "/usr/bin:/bin:/usr/sbin:/sbin"


def write_exe(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o755)


class InstallLocalTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build_dir = tempfile.mkdtemp()
        cls.pool_bin = Path(cls.build_dir) / "codex-pool"
        subprocess.run(["go", "build", "-o", str(cls.pool_bin), "./cmd/codex-pool"],
                       cwd=REPO, check=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.build_dir, ignore_errors=True)

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.home = self.tmp
        self.prefix = self.tmp / "bin"
        self.old_codex = self.tmp / "oldbin" / "codex"
        write_exe(self.old_codex, "#!/bin/sh\necho old codex\n")
        self.pool_home = self.home / ".codex-pool"

    def run_script(self, *args, path_dirs=None, check=True, service=False):
        dirs = path_dirs if path_dirs is not None else [self.old_codex.parent]
        env = {
            "HOME": str(self.home),
            "PATH": ":".join([str(d) for d in dirs] + [SYSTEM_PATH]),
            "CODEX_POOL_BIN": str(self.pool_bin),
        }
        if not service:
            env["CODEX_POOL_NO_SERVICE"] = "1"
        result = subprocess.run(["sh", str(SCRIPT), "--prefix", str(self.prefix), *args],
                                env=env, capture_output=True, text=True)
        if check and result.returncode != 0:
            self.fail(f"install failed ({result.returncode}):\n{result.stdout}\n{result.stderr}")
        return result

    def wrapper_text(self):
        return ("#!/bin/sh\n"
                "# Normal Codex is deliberately not routed through codex-pool.\n"
                f'exec "{self.old_codex}" "$@"\n')

    def config(self):
        return json.loads((self.pool_home / "pool.json").read_text())

    def backups(self):
        return sorted(p.name for p in self.prefix.glob("codex.pre-pool-*"))

    def test_fresh_install_writes_wrapper_binary_and_config(self):
        result = self.run_script()
        wrapper = self.prefix / "codex"
        self.assertEqual(wrapper.read_text(), self.wrapper_text())
        self.assertEqual(stat.S_IMODE(wrapper.stat().st_mode), 0o755)
        self.assertTrue(os.access(self.prefix / "codex-pool", os.X_OK))
        cfg = self.config()
        self.assertNotIn("round_robin_endpoints", cfg)
        self.assertEqual(cfg["accounts_dir"], str(self.home / ".codex-accounts"))
        self.assertEqual(cfg["codex_home"], str(self.home / ".codex"))
        self.assertEqual(cfg["codex_bin"], str(self.old_codex))
        self.assertEqual(cfg["listen"], "127.0.0.1:18473")
        self.assertTrue((self.pool_home / "state" / "client.key").is_file())
        self.assertTrue((self.home / ".codex-accounts").is_dir())
        self.assertEqual(self.backups(), [])
        self.assertIn("base URL: http://127.0.0.1:18473/_pool/rr", result.stdout)
        self.assertIn(f"key file: {self.pool_home}/state/client.key", result.stdout)
        self.assertNotIn("KB_", result.stdout)
        self.assertNotIn("kb-pool.json", result.stdout)
        self.assertNotIn("/kb/", result.stdout)
        self.assertFalse((self.pool_home / "kb-pool.json").exists())
        self.assertIn(f'"{self.prefix}/codex-pool" account add main --from', result.stdout)
        self.assertIn("account add main --from", result.stdout)
        self.assertFalse((self.home / "Library").exists())
        self.assertFalse((self.home / ".config").exists())

    def test_rr_config_is_written_once(self):
        result = self.run_script()
        rr = self.pool_home / "rr.json"
        self.assertEqual(json.loads(rr.read_text()), {
            "base_url": "http://127.0.0.1:18473/_pool/rr",
            "key_file": str(self.pool_home / "state" / "client.key"),
        })
        self.assertEqual(stat.S_IMODE(rr.stat().st_mode), 0o600)
        self.assertIn(str(rr), result.stdout)
        before = rr.read_bytes()
        again = self.run_script()
        self.assertEqual(rr.read_bytes(), before)
        self.assertIn(f"keeping existing {rr}", again.stdout)
        custom = b'{"base_url": "http://127.0.0.1:1/_pool/rr", "key_file": "/x"}\n'
        rr.write_bytes(custom)
        kept = self.run_script()
        self.assertEqual(rr.read_bytes(), custom)
        self.assertIn("base URL: http://127.0.0.1:1/_pool/rr", kept.stdout)
        self.assertIn("key file: /x", kept.stdout)

    def test_rr_config_follows_listen(self):
        self.run_script()
        cfg = self.config()
        cfg["listen"] = ":18999"
        (self.pool_home / "pool.json").write_text(json.dumps(cfg, indent=2) + "\n")
        (self.pool_home / "rr.json").unlink()
        self.run_script()
        self.assertEqual(json.loads((self.pool_home / "rr.json").read_text())["base_url"],
                         "http://127.0.0.1:18999/_pool/rr")

    def test_legacy_kb_pool_config_is_carried_over_and_kept(self):
        self.skipTest("legacy kb-pool configuration was removed")
        self.pool_home.mkdir()
        legacy = self.pool_home / "kb-pool.json"
        content = b'{"origin": "http://127.0.0.1:18555/", "key_file": "/legacy/client.key"}\n'
        legacy.write_bytes(content)
        result = self.run_script()
        self.assertEqual(json.loads((self.pool_home / "rr.json").read_text()), {
            "base_url": "http://127.0.0.1:18555/_pool/rr",
            "key_file": "/legacy/client.key",
        })
        self.assertEqual(legacy.read_bytes(), content)
        notices = [line for line in (result.stdout + result.stderr).splitlines() if "kb-pool.json" in line]
        self.assertEqual(len(notices), 1, notices)
        again = self.run_script()
        self.assertNotIn("kb-pool.json", again.stdout + again.stderr)

    def test_pool_rr_wrapper_uses_local_rr_config(self):
        self.run_script()
        wrapper = self.prefix / "pool-rr"
        self.assertEqual(stat.S_IMODE(wrapper.stat().st_mode), 0o755)
        text = wrapper.read_text()
        self.assertTrue(text.startswith("#!/bin/sh\n"))
        self.assertIn(f'"{REPO}/scripts/pool-rr.py"', text)
        self.assertIn(f'--config "{self.pool_home}/rr.json"', text)
        self.assertIn('"$@"', text)
        self.run_script()
        self.assertEqual(wrapper.read_text(), text)

    def test_existing_pool_rr_link_is_moved_aside(self):
        self.prefix.mkdir()
        (self.prefix / "pool-rr").symlink_to(REPO / "scripts" / "pool-rr.py")
        self.run_script()
        backups = sorted(p.name for p in self.prefix.glob("pool-rr.pre-pool-*"))
        self.assertEqual(len(backups), 1)
        self.assertTrue((self.prefix / backups[0]).is_symlink())
        self.assertFalse((self.prefix / "pool-rr").is_symlink())
        self.run_script()
        self.assertEqual(sorted(p.name for p in self.prefix.glob("pool-rr.pre-pool-*")), backups)

    def test_rerun_is_idempotent_even_with_wrapper_first_on_path(self):
        self.run_script()
        before = (self.pool_home / "pool.json").read_bytes()
        self.run_script(path_dirs=[self.prefix, self.old_codex.parent])
        self.assertEqual((self.pool_home / "pool.json").read_bytes(), before)
        self.assertIn("# Normal Codex is deliberately not routed through codex-pool.",
                      (self.prefix / "codex").read_text())
        self.assertEqual(self.backups(), [])

    def test_existing_real_codex_in_prefix_is_moved_aside(self):
        real = self.tmp / "convenient-codex"
        write_exe(real, "#!/bin/sh\necho real\n")
        write_exe(self.prefix / "codex", f'#!/bin/sh\nexec "{real}" "$@"\n')
        self.run_script("--codex-bin", str(real), path_dirs=[self.prefix])
        self.assertEqual(self.config()["codex_bin"], str(real))
        self.assertIn("# Normal Codex is deliberately not routed through codex-pool.",
                      (self.prefix / "codex").read_text())
        backups = self.backups()
        self.assertEqual(len(backups), 1)
        self.assertIn(str(real), (self.prefix / backups[0]).read_text())
        self.run_script("--codex-bin", str(real), path_dirs=[self.prefix])
        self.assertEqual(self.backups(), backups)

    def test_symlinked_codex_in_prefix_does_not_hide_its_target(self):
        self.prefix.mkdir()
        (self.prefix / "codex").symlink_to(self.old_codex)
        self.run_script(path_dirs=[self.prefix, self.old_codex.parent])
        self.assertEqual(self.config()["codex_bin"], str(self.old_codex))
        backups = self.backups()
        self.assertEqual(len(backups), 1)
        self.assertTrue((self.prefix / backups[0]).is_symlink())
        self.assertEqual((self.prefix / "codex").read_text(), self.wrapper_text())

    def assert_rejected_wrapper_bin(self, codex_bin):
        result = self.run_script("--codex-bin", str(codex_bin), path_dirs=[], check=False)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("wrapper", result.stderr)
        self.assertIn(str(self.old_codex), result.stderr)  # hint: the symlink's target
        self.assertFalse((self.pool_home / "pool.json").exists())
        self.assertTrue((self.prefix / "codex").is_symlink())  # nothing moved aside

    def test_codex_bin_at_wrapper_location_is_rejected(self):
        self.prefix.mkdir()
        (self.prefix / "codex").symlink_to(self.old_codex)
        self.assert_rejected_wrapper_bin(self.prefix / "codex")

    def test_codex_bin_linking_through_wrapper_location_is_rejected(self):
        self.prefix.mkdir()
        (self.prefix / "codex").symlink_to(self.old_codex)
        via = self.tmp / "via" / "codex"
        via.parent.mkdir()
        via.symlink_to(self.prefix / "codex")
        self.assert_rejected_wrapper_bin(via)

    def test_detection_skips_links_to_wrapper_location(self):
        self.run_script()
        link = self.tmp / "linkbin" / "codex"
        link.parent.mkdir()
        link.symlink_to(self.prefix / "codex")
        shutil.rmtree(self.pool_home)
        self.run_script(path_dirs=[link.parent, self.old_codex.parent])
        self.assertEqual(self.config()["codex_bin"], str(self.old_codex))

    def test_explicit_codex_bin_differing_from_existing_config_warns(self):
        self.run_script()
        before = (self.pool_home / "pool.json").read_bytes()
        other = self.tmp / "other" / "codex"
        write_exe(other, "#!/bin/sh\n")
        result = self.run_script("--codex-bin", str(other))
        self.assertEqual((self.pool_home / "pool.json").read_bytes(), before)
        self.assertIn("--codex-bin", result.stderr)
        self.assertIn(str(self.old_codex), result.stderr)
        self.assertIn(str(self.pool_home / "pool.json"), result.stderr)
        same = self.run_script("--codex-bin", str(self.old_codex))
        self.assertNotIn("--codex-bin", same.stderr)

    def test_reports_when_no_codex_is_on_path(self):
        result = self.run_script("--codex-bin", str(self.old_codex), path_dirs=[])
        self.assertIn("(none on PATH)", result.stdout)

    def test_detection_skips_other_pool_wrappers(self):
        other = self.tmp / "otherbin" / "codex"
        write_exe(other, '#!/bin/sh\nexec codex-pool launch --config x -- "$@"\n')
        self.run_script(path_dirs=[other.parent, self.old_codex.parent])
        self.assertEqual(self.config()["codex_bin"], str(self.old_codex))

    def test_service_is_rendered_and_started_with_fake_service_manager(self):
        system = platform.system()
        if system not in ("Darwin", "Linux"):
            self.skipTest("service only on macOS and Linux")
        fake = self.tmp / "fakebin"
        calls = self.tmp / "calls.log"
        tool = "launchctl" if system == "Darwin" else "systemctl"
        # "print" reports the job as loaded so the bootout path runs too.
        write_exe(fake / tool, f'#!/bin/sh\necho "$*" >>"{calls}"\nexit 0\n')
        self.run_script(path_dirs=[fake, self.old_codex.parent], service=True)
        log = calls.read_text()
        cfg = str(self.pool_home / "pool.json")
        serve_log = str(self.pool_home / "state" / "serve.log")
        if system == "Darwin":
            plist = self.home / "Library/LaunchAgents/com.local.codex-pool.plist"
            with plist.open("rb") as f:
                job = plistlib.load(f)
            self.assertEqual(job["Label"], "com.local.codex-pool")
            self.assertEqual(job["ProgramArguments"],
                             [str(self.prefix / "codex-pool"), "serve", "--config", cfg])
            self.assertTrue(job["KeepAlive"])
            self.assertTrue(job["RunAtLoad"])
            self.assertEqual(job["StandardOutPath"], serve_log)
            self.assertEqual(job["StandardErrorPath"], serve_log)
            uid = os.getuid()
            self.assertIn(f"bootout gui/{uid}/com.local.codex-pool", log)
            self.assertIn(f"bootstrap gui/{uid} {plist}", log)
        else:
            unit = (self.home / ".config/systemd/user/codex-pool.service").read_text()
            self.assertIn(f'ExecStart="{self.prefix}/codex-pool" serve --config "{cfg}"', unit)
            self.assertIn(f"StandardOutput=append:{serve_log}", unit)
            self.assertIn("--user daemon-reload", log)
            self.assertIn("--user enable --now codex-pool", log)
        rendered = plist.read_text() if system == "Darwin" else unit
        self.assertIsNone(re.search(r"@[A-Z_]+@", rendered))

    def test_missing_codex_fails_without_writing_config(self):
        if any(Path(d, "codex").exists() for d in SYSTEM_PATH.split(":")):
            self.skipTest("codex exists in the system PATH")
        result = self.run_script(path_dirs=[], check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--codex-bin", result.stderr)
        self.assertFalse((self.pool_home / "pool.json").exists())


if __name__ == "__main__":
    unittest.main()
