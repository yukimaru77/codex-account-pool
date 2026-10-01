import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("pool-rr.py")


class PoolRRTest(unittest.TestCase):
    def test_standalone_search_capability_requires_explicit_deployed_route_opt_in(self):
        spec = importlib.util.spec_from_file_location("pool_rr", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "client.key").write_text("fixture-key")
            (root / "models.json").write_text('{"models":[]}')
            config = root / "bridge.json"
            settings = {"origin": "http://127.0.0.1:18473", "key_file": "client.key",
                        "rr_model_catalog": str(root / "models.json")}
            for enabled in (None, False, True):
                if enabled is not None:
                    settings["rr_standalone_web_search"] = enabled
                config.write_text(json.dumps(settings))
                argv, env = module.command(config, ["codex", "exec", "--ignore-user-config", "-c", 'web_search="live"', "find a paper"])
                provider = next(arg for arg in argv if arg.startswith("model_providers.pool_rr="))
                self.assertEqual("supports_standalone_web_search = true" in provider, enabled is True)
                self.assertIn('web_search="live"', argv)
                self.assertNotIn("fixture-key", " ".join(argv))
            settings["rr_standalone_web_search"] = "false"
            config.write_text(json.dumps(settings))
            with self.assertRaisesRegex(ValueError, "boolean"):
                module.command(config, ["codex", "exec", "hello"])

    def test_invocation_preserves_arguments_stdin_exit_and_scopes_secret(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "keys").mkdir()
            (root / "keys/client.key").write_text("test-client-secret\n")
            config = root / "bridge.json"
            (root / "models.json").write_text('{"models":[]}')
            config.write_text(json.dumps({"origin": "http://127.0.0.1:18473", "key_file": "keys/client.key",
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

    def test_default_config_prefers_rr_json_over_legacy_kb_pool_json(self):
        spec = importlib.util.spec_from_file_location("pool_rr", SCRIPT)
        pool_rr = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(pool_rr)
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            with mock.patch.dict(os.environ, {"HOME": str(home)}):
                self.assertRaises(ValueError, pool_rr.default_config)
                (home / ".codex-pool").mkdir()
                legacy = home / ".codex-pool/kb-pool.json"
                legacy.write_text("{}")
                self.assertEqual(pool_rr.default_config(), legacy)
                local = home / ".codex-pool/rr.json"
                local.write_text("{}")
                self.assertEqual(pool_rr.default_config(), local)

    def run_default(self, name, settings):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            state = home / ".codex-pool/state"
            state.mkdir(parents=True)
            (state / "client.key").write_text("test-client-secret\n")
            (home / ".codex-pool" / name).write_text(json.dumps(
                dict(settings, key_file=str(state / "client.key"))))
            (home / ".codex").mkdir()
            (home / ".codex/models_cache.json").write_text('{"models":[]}')
            codex = home / "codex"
            codex.write_text("#!" + sys.executable + "\nimport json,sys\nprint(json.dumps(sys.argv[1:]))\n")
            codex.chmod(0o755)
            env = dict(os.environ, HOME=str(home), PATH=str(home) + os.pathsep + os.environ["PATH"])
            env.pop("CODEX_HOME", None)
            return subprocess.run([sys.executable, str(SCRIPT), "codex", "exec", "hi"],
                                  capture_output=True, text=True, env=env)

    def test_runs_with_local_rr_json_by_default(self):
        for base in ("http://127.0.0.1:18999/_pool/rr", "http://127.0.0.1:18999/_pool/rr/"):
            with self.subTest(base=base):
                result = self.run_default("rr.json", {"base_url": base})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('"http://127.0.0.1:18999/_pool/rr"', json.loads(result.stdout)[4])

    def test_runs_with_legacy_kb_pool_json_origin(self):
        result = self.run_default("kb-pool.json", {"origin": "http://127.0.0.1:18998"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('"http://127.0.0.1:18998/_pool/rr"', json.loads(result.stdout)[4])

    def test_rejects_base_url_outside_rr_routes(self):
        for settings in ({"base_url": "http://127.0.0.1:18999"}, {"base_url": "http://127.0.0.1:18999/v1"},
                         {"base_url": "http://pool.example:18999/_pool/rr"}, {}):
            with self.subTest(settings=settings):
                result = self.run_default("rr.json", settings)
                self.assertEqual(result.returncode, 1)
                self.assertNotIn("test-client-secret", result.stdout + result.stderr)

    def test_rejects_commands_outside_supported_invocations(self):
        for argv in ([], ["codex"], ["other", "codex"], ["other", "exec"], ["codex", "login"]):
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
