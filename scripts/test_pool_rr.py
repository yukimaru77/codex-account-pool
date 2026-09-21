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

    def test_kb_preserves_invocation_and_sets_one_pool_for_binding_and_inference(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "pool state").mkdir()
            key_file = root / "pool state/client.key"
            key_file.write_text("test-client-secret\n")
            catalog = root / "models.json"
            catalog.write_text('{"models":[]}')
            config = root / "bridge.json"
            kb = root / "kb"
            kb.write_text("#!" + sys.executable + "\n" +
                          "import os,sys,json\n" +
                          "print(json.dumps({'args':sys.argv[1:], 'stdin':sys.stdin.read(), " +
                          "'key_ok':os.environ.get('CODEX_POOL_RR_KEY') == 'test-client-secret', " +
                          "'config':os.environ.get('KB_CODEX_CONFIG_OVERRIDES'), " +
                          "'origin':os.environ.get('KB_POOL_ORIGIN'), " +
                          "'key_file':os.environ.get('KB_POOL_KEY_FILE'), " +
                          "'private_http':os.environ.get('KB_POOL_PRIVATE_HTTP')}))\n" +
                          "sys.exit(23)\n")
            kb.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                       KB_POOL_ORIGIN="http://stale-pool:18473",
                       KB_POOL_KEY_FILE="/stale/client.key", KB_POOL_PRIVATE_HTTP="1",
                       KB_CODEX_CONFIG_OVERRIDES='["model_provider=\\\"stale\\\""]')
            before = dict(os.environ)
            for kb_args in (["paper-demo", "--remote", "codex"],
                            ["paper-demo", "--store", "research", "--remote", "codex", "exec",
                             "-m", "example-model", "question with spaces"],
                            ["paper-demo", "codex", "--", "exec", "--json", "-"],
                            ["codex", "paper-demo", "--remote", "exec", "legacy syntax"]):
                for private_http in (False, True):
                    with self.subTest(kb_args=kb_args, private_http=private_http):
                        origin = "http://pool.example:18473/" if private_http else "http://127.0.0.1:18473/"
                        settings = {"origin": origin, "state_dir": "pool state",
                                    "rr_model_catalog": str(catalog), "private_http": private_http}
                        config.write_text(json.dumps(settings))
                        argv = [str(kb), *kb_args]
                        result = subprocess.run([sys.executable, str(SCRIPT), "--config", str(config), *argv],
                                                input="stdin material", text=True, capture_output=True, env=env)
                        self.assertEqual(result.returncode, 23, result.stderr)
                        data = json.loads(result.stdout)
                        self.assertEqual(data["args"], argv[1:])
                        self.assertEqual(data["stdin"], "stdin material")
                        self.assertTrue(data["key_ok"])
                        self.assertEqual(data["origin"], origin.rstrip("/"))
                        self.assertEqual(data["key_file"], str(key_file.resolve()))
                        self.assertEqual(data["private_http"], "1" if private_http else "0")
                        overrides = json.loads(data["config"])
                        self.assertEqual(len(overrides), 3)
                        self.assertEqual(overrides[0], 'model_provider="pool_rr"')
                        self.assertEqual(overrides[2], "model_catalog_json=" + json.dumps(str(catalog.resolve())))
                        self.assertTrue(overrides[1].startswith("model_providers.pool_rr={ "))
                        for setting in ('base_url = "' + origin.rstrip("/") + '/_pool/rr"',
                                        'env_key = "CODEX_POOL_RR_KEY"',
                                        "requires_openai_auth = false", "supports_websockets = false",
                                        "request_max_retries = 0", "stream_max_retries = 0"):
                            self.assertIn(setting, overrides[1])
                        self.assertNotIn("test-client-secret", result.stdout + result.stderr)
                        self.assertEqual(json.loads(config.read_text()), settings)
            self.assertEqual(dict(os.environ), before)

    def test_rejects_commands_outside_supported_invocations(self):
        for argv in ([], ["codex"], ["kb", "create", "demo"], ["other", "codex"], ["codex", "login"],
                     ["kb", "codex"], ["kb", "--remote", "codex"], ["kb", "codex", "--remote"]):
            with self.subTest(argv=argv):
                result = subprocess.run([sys.executable, str(SCRIPT), *argv], capture_output=True, text=True)
                self.assertEqual(result.returncode, 1)

    def test_rejects_remote_plain_http_without_opt_in(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "bridge.json"
            config.write_text('{"origin":"http://pool.example:18473"}')
            result = subprocess.run([sys.executable, str(SCRIPT), "--config", str(config),
                                     "codex", "exec", "hello"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)


if __name__ == "__main__":
    unittest.main()
