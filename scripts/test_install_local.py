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
                "# codex-pool launch: picks CODEX_HOME by remaining quota.\n"
                f'exec "{self.prefix}/codex-pool" launch --config "{self.pool_home}/pool.json" -- "$@"\n')

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
        self.assertIn("KB_POOL_ORIGIN=http://127.0.0.1:18473", result.stdout)
        self.assertIn(f"KB_POOL_KEY_FILE={self.pool_home}/state/client.key", result.stdout)
        self.assertIn("account add main --from", result.stdout)
        self.assertFalse((self.home / "Library").exists())
        self.assertFalse((self.home / ".config").exists())

    def test_rerun_is_idempotent_even_with_wrapper_first_on_path(self):
        self.run_script()
        before = (self.pool_home / "pool.json").read_bytes()
        self.run_script(path_dirs=[self.prefix, self.old_codex.parent])
        self.assertEqual((self.pool_home / "pool.json").read_bytes(), before)
        self.assertEqual((self.prefix / "codex").read_text(), self.wrapper_text())
        self.assertEqual(self.backups(), [])

    def test_existing_real_codex_in_prefix_is_moved_aside(self):
        real = self.tmp / "convenient-codex"
        write_exe(real, "#!/bin/sh\necho real\n")
        write_exe(self.prefix / "codex", f'#!/bin/sh\nexec "{real}" "$@"\n')
        self.run_script("--codex-bin", str(real), path_dirs=[self.prefix])
        self.assertEqual(self.config()["codex_bin"], str(real))
        self.assertEqual((self.prefix / "codex").read_text(), self.wrapper_text())
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
