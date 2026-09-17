"""Check native session assertions with a disposable app-server, never real login."""

import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest


FAKE = '''
import json, os, pathlib, sys
assert sys.argv[1:] == ["app-server"]
assert os.environ.get("PYTHONUNBUFFERED") == UNBUFFERED
def emit(v):
    print(json.dumps(v), flush=True)
def done(tid, status="completed"):
    emit({"method":"turn/completed", "params":{"turn":{"id":tid,"status":status}}})
count=0
for line in sys.stdin:
    q=json.loads(line)
    method=q["method"]
    p=q["params"]
    result={}
    if method=="initialized": continue
    if method=="thread/start":
        assert set(p)=={"cwd","ephemeral"}
        result={"thread":{"id":"fixture-thread"}}
    if method=="turn/start":
        assert set(p)=={"threadId","input"}
        count+=1
        tid=str(count)
        text=p["input"][0]["text"]
        result={"turn":{"id":tid}}
        answer=""
        if "POOL_TOOL_OK" in text:
            pathlib.Path("relay-marker.txt").write_text("POOL_TOOL_OK")
            answer="POOL_TOOL_OK"
        elif "SAVED" in text: answer="SAVED"
        elif "remember" in text: answer="SAPPHIRE_POOL_724"
        elif "POOL_RECOVERY_OK" in text: answer="POOL_RECOVERY_OK"
        if answer:
            emit({"method":"item/completed","params":{"item":{"type":"agentMessage","text":"WRONG" if WRONG else answer}}})
            # Completion deliberately precedes the RPC result.
            done(tid)
        else:
            emit({"method":"item/agentMessage/delta","params":{"delta":"One"}})
    if method=="thread/compact/start":
        emit({"method":"item/completed","params":{"item":{"type":"contextCompaction"}}})
        done("compact")
    if method=="turn/interrupt": done(p["turnId"],"interrupted")
    emit({"id":q["id"],"result":result})
'''


class NativeSessionTest(unittest.TestCase):
    def check(self, scenario, wrong=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "fixture-home"
            home.mkdir()
            (home / "config.toml").write_text("# fixture only\n")
            config = root / "pool.json"
            config.write_text(json.dumps({"state_dir":"state"}))
            executable = root / "fake-codex"
            executable.write_text("#!" + sys.executable + "\nWRONG=" + repr(wrong) + "\nUNBUFFERED=" + repr(os.environ.get("PYTHONUNBUFFERED")) + "\n" + FAKE)
            executable.chmod(0o700)
            result = subprocess.run([sys.executable, str(Path(__file__).with_name("check-native-session.py")),
                                     "direct", "--scenario", scenario, "--config", str(config), "--codex", str(executable)],
                                    env=dict(os.environ, CODEX_HOME=str(home)), capture_output=True, text=True, timeout=20)
            reports = list((root / "state/checks").glob("*/result.json"))
            self.assertEqual(len(reports), 1, result.stderr)
            report = json.loads(reports[0].read_text())
            self.assertEqual(result.returncode, int(wrong), result.stdout + result.stderr)
            self.assertEqual(report["ok"], not wrong)
            self.assertTrue(report["hashes_unchanged"])
            self.assertFalse(report["bridge_observed_native_request"])
            self.assertEqual((home / "config.toml").read_text(), "# fixture only\n")
            for name in ("events.jsonl", "stderr.log", "bridge.log", "result.json"):
                self.assertEqual(stat.S_IMODE((reports[0].parent / name).stat().st_mode), 0o600)

    def test_tools(self): self.check("tools")
    def test_compaction_before_rpc_result(self): self.check("compact")
    def test_interrupt_before_rpc_result(self): self.check("interrupt")
    def test_wrong_answer_fails(self): self.check("tools", wrong=True)


if __name__ == "__main__":
    unittest.main()
