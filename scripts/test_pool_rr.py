import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("pool-rr.py")


class PoolRRTest(unittest.TestCase):
    def test_invocation_preserves_arguments_stdin_exit_and_scopes_secret(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "state").mkdir()
            (root / "state/client.key").write_text("test-client-secret\n")
            config = root / "bridge.json"
            (root / "models.json").write_text('{"models":[]}')
            config.write_text(json.dumps({"origin": "http://127.0.0.1:18473", "state_dir": "state",
                                          "rr_model_catalog": str(root / "models.json")}))
            codex = root / "codex"
            codex.write_text("#!" + sys.executable + "\n" +
                             "import os,sys,json\n" +
                             "print(json.dumps({'args':sys.argv[1:], 'stdin':sys.stdin.read(), " +
                             "'key_ok':os.environ.get('CODEX_POOL_RR_KEY') == 'test-client-secret'}))\n" +
                             "sys.exit(23)\n")
            codex.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"])
            before = os.environ.get("CODEX_POOL_RR_KEY")
            result = subprocess.run([sys.executable, str(SCRIPT), "--config", str(config),
                                     "codex", "exec", "-m", "example-model", "prompt with spaces"],
                                    input="stdin material", text=True, capture_output=True, env=env)
            self.assertEqual(result.returncode, 23, result.stderr)
            data = json.loads(result.stdout)
            self.assertTrue(data["key_ok"])
            self.assertEqual(data["stdin"], "stdin material")
            self.assertEqual(data["args"][0], "exec")
            self.assertEqual(data["args"][-3:], ["-m", "example-model", "prompt with spaces"])
            self.assertIn('model_provider="pool_rr"', data["args"])
            provider = data["args"][4]
            self.assertIn('"http://127.0.0.1:18473/_pool/rr"', provider)
            self.assertIn('supports_websockets = false', provider)
            self.assertNotIn("test-client-secret", result.stdout + result.stderr)
            self.assertEqual(os.environ.get("CODEX_POOL_RR_KEY"), before)
            self.assertEqual(json.loads(config.read_text())["origin"], "http://127.0.0.1:18473")

    def test_rejects_remote_plain_http_without_opt_in(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "bridge.json"
            config.write_text('{"origin":"http://pool.example:18473"}')
            result = subprocess.run([sys.executable, str(SCRIPT), "--config", str(config),
                                     "codex", "exec", "hello"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)


if __name__ == "__main__":
    unittest.main()
