#!/usr/bin/env python3
"""Adversarial, offline tests for the release CI gate and both publish paths."""
import copy
import importlib.util
import io
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError


ROOT = Path(__file__).resolve().parent.parent
sys.dont_write_bytecode = True
SPEC = importlib.util.spec_from_file_location("release_ci", ROOT / "hack/check-release-ci.py")
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)
SHA = "a" * 40
MAIN = "b" * 40
TAG_OBJECT = "c" * 40
TAG = "v1.2.3"


class FakeGitHub(GATE.GitHub):
    """Exercise real pagination/selection logic against an in-memory REST API."""
    def __init__(self):
        self.calls = []
        self.runs = []
        self.jobs = {}
        self.refs = {
            "/git/ref/tags/" + TAG: {"object": {"type": "tag", "sha": TAG_OBJECT}},
            "/git/tags/" + TAG_OBJECT: {"object": {"type": "commit", "sha": SHA}},
            "/git/ref/heads/main": {"object": {"type": "commit", "sha": MAIN}},
            "/compare/" + SHA + "..." + MAIN: {"status": "ahead", "merge_base_commit": {"sha": SHA}},
        }
        for number, (path, names) in enumerate(GATE.REQUIRED.items(), 1):
            self.runs.append({
                "id": number, "run_number": number, "run_attempt": 2,
                "path": path, "head_sha": SHA, "head_branch": "main", "event": "push",
                "repository": {"full_name": GATE.REPOSITORY},
                "status": "completed", "conclusion": "success",
            })
            self.jobs[number] = [self.job(number, name, i) for i, name in enumerate(sorted(names), 1)]

    @staticmethod
    def job(run, name, job_id):
        return {"id": run * 1000 + job_id, "run_id": run, "run_attempt": 2,
                "head_sha": SHA, "name": name, "status": "completed", "conclusion": "success"}

    def get(self, path, **query):
        self.calls.append((path, query))
        if path in self.refs:
            return copy.deepcopy(self.refs[path])
        if path == "/actions/runs":
            assert query["head_sha"] == SHA and query["branch"] == "main"
            key, values = "workflow_runs", self.runs
        elif path.endswith("/jobs"):
            parts = path.split("/")
            assert parts[4] == "attempts" and parts[5] == "2", path
            key, values = "jobs", self.jobs[int(parts[3])]
        else:
            run_id = int(path.split("/")[-1])
            return copy.deepcopy(next(run for run in self.runs if run["id"] == run_id))
        start = (query["page"] - 1) * query["per_page"]
        return {"total_count": len(values), key: copy.deepcopy(values[start:start + query["per_page"]])}


class GateTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeGitHub()

    def check(self):
        return GATE.check(self.api, TAG, SHA)

    def test_complete_latest_attempts_pass(self):
        self.assertEqual(len(self.check()), 5)
        self.assertTrue(any("/attempts/2/jobs" in path for path, _ in self.api.calls))

    def test_lightweight_tag_passes(self):
        self.api.refs["/git/ref/tags/" + TAG]["object"] = {"type": "commit", "sha": SHA}
        self.check()

    def test_invalid_tag_and_short_sha_fail_without_api_requests(self):
        for tag, revision in (("main", SHA), (TAG, SHA[:7])):
            with self.assertRaises(GATE.GateError):
                GATE.check(self.api, tag, revision)
        self.assertEqual(self.api.calls, [])

    def test_nested_or_invalid_tag_objects_fail_closed(self):
        for kind in ("tag", "tree"):
            self.setUp()
            self.api.refs["/git/tags/" + TAG_OBJECT]["object"] = {"type": kind, "sha": TAG_OBJECT}
            with self.assertRaises(GATE.GateError):
                self.check()

    def test_wrong_tag_commit_and_non_main_history_fail(self):
        for edit in ("tag", "ancestry", "merge_base"):
            with self.subTest(edit=edit):
                self.setUp()
                if edit == "tag":
                    self.api.refs["/git/tags/" + TAG_OBJECT]["object"]["sha"] = MAIN
                elif edit == "ancestry":
                    self.api.refs["/compare/" + SHA + "..." + MAIN]["status"] = "diverged"
                else:
                    self.api.refs["/compare/" + SHA + "..." + MAIN]["merge_base_commit"]["sha"] = MAIN
                with self.assertRaises(GATE.GateError):
                    self.check()

    def test_missing_workflow_or_job_fails(self):
        for omitted in ("workflow", "job", "all_jobs", "old_e2e"):
            with self.subTest(omitted=omitted):
                self.setUp()
                if omitted == "workflow":
                    self.api.runs.pop()
                elif omitted == "all_jobs":
                    self.api.jobs[1] = []
                elif omitted == "old_e2e":
                    self.api.jobs[4] = [self.api.job(4, "Run on Ubuntu", 1)]
                else:
                    self.api.jobs[1].pop()
                with self.assertRaises(GATE.GateError):
                    self.check()

    def test_every_non_success_state_fails_at_workflow_and_job_level(self):
        states = [("completed", conclusion) for conclusion in
                  ("failure", "skipped", "cancelled", "timed_out", "neutral", "action_required", "stale", None)]
        states += [(status, None) for status in ("queued", "in_progress", "waiting", "pending", "requested")]
        states += [("in_progress", "success")]
        for level in ("workflow", "job"):
            for status, conclusion in states:
                with self.subTest(level=level, status=status, conclusion=conclusion):
                    self.setUp()
                    obj = self.api.runs[0] if level == "workflow" else self.api.jobs[1][0]
                    obj.update(status=status, conclusion=conclusion)
                    with self.assertRaises(GATE.GateError):
                        self.check()

    def test_wrong_source_or_job_identity_fails(self):
        edits = [
            ("run", "head_sha", MAIN), ("run", "head_branch", "feature"),
            ("run", "event", "pull_request"),
            ("run", "repository", {"full_name": "someone/fork"}),
            ("job", "head_sha", MAIN), ("job", "run_id", 99), ("job", "run_attempt", 1),
        ]
        for level, key, value in edits:
            with self.subTest(level=level, key=key):
                self.setUp()
                obj = self.api.runs[0] if level == "run" else self.api.jobs[1][0]
                obj[key] = value
                with self.assertRaises(GATE.GateError):
                    self.check()

    def test_newer_run_overrides_old_success_regardless_of_api_order(self):
        for reverse in (False, True):
            self.setUp()
            newer = copy.deepcopy(self.api.runs[0])
            newer.update(id=99, run_number=99, status="queued", conclusion=None)
            self.api.runs.append(newer)
            if reverse:
                self.api.runs.reverse()
            with self.assertRaises(GATE.GateError):
                self.check()

    def test_newer_success_supersedes_old_failure(self):
        old = copy.deepcopy(self.api.runs[0])
        old.update(id=99, run_number=0, conclusion="failure")
        self.api.runs.append(old)
        self.check()

    def test_tag_or_pr_success_does_not_replace_failed_main_run(self):
        for branch, event in ((TAG, "push"), ("feature", "pull_request")):
            self.setUp()
            unrelated = copy.deepcopy(self.api.runs[0])
            unrelated.update(id=99, run_number=99, head_branch=branch, event=event)
            self.api.runs.append(unrelated)
            self.api.runs[0]["conclusion"] = "failure"
            with self.assertRaises(GATE.GateError):
                self.check()

    def test_new_scheduled_vulnerability_failure_blocks(self):
        newer = copy.deepcopy(self.api.runs[-1])
        newer.update(id=99, run_number=99, event="schedule", conclusion="failure")
        self.api.runs.append(newer)
        with self.assertRaises(GATE.GateError):
            self.check()

    def test_stale_success_cannot_mask_a_new_attempt(self):
        original = self.api.get
        def get(path, **query):
            result = original(path, **query)
            if path == "/actions/runs/1":
                result.update(run_attempt=3, status="in_progress", conclusion=None)
            return result
        self.api.get = get
        with self.assertRaises(GATE.GateError):
            self.check()

    def test_attempt_changes_after_jobs_fail(self):
        original = self.api.get
        reads = 0
        def get(path, **query):
            nonlocal reads
            result = original(path, **query)
            if path == "/actions/runs/1":
                reads += 1
                if reads == 2:
                    result["run_attempt"] = 3
            return result
        self.api.get = get
        with self.assertRaisesRegex(GATE.GateError, "attempt changed"):
            self.check()

    def test_new_run_while_reading_jobs_fails(self):
        original = self.api.get
        reads = 0
        def get(path, **query):
            nonlocal reads
            if path == "/actions/runs":
                reads += 1
                if reads == 2:
                    newer = copy.deepcopy(self.api.runs[0])
                    newer.update(id=99, run_number=99)
                    self.api.runs.append(newer)
            return original(path, **query)
        self.api.get = get
        with self.assertRaisesRegex(GATE.GateError, "CI changed"):
            self.check()

    def test_runs_and_jobs_are_paginated(self):
        for number in range(10, 115):
            extra = copy.deepcopy(self.api.runs[0])
            extra.update(id=number, path=".github/workflows/unrelated.yml")
            self.api.runs.insert(0, extra)
        for number in range(10, 115):
            self.api.jobs[1].append(self.api.job(1, f"extra-{number}", number))
        self.check()
        self.assertTrue(any(path == "/actions/runs" and query["page"] == 2 for path, query in self.api.calls))
        self.assertTrue(any(path.endswith("/jobs") and query["page"] == 2 for path, query in self.api.calls))
        # A failure beyond page one must also block, even in an added job.
        self.api.jobs[1][-1]["conclusion"] = "failure"
        with self.assertRaises(GATE.GateError):
            self.check()

    def test_incomplete_or_inconsistent_pages_fail(self):
        for mode in ("truncated", "count_changed", "duplicate", "invalid_count"):
            with self.subTest(mode=mode):
                pages = [
                    {"total_count": 2, "jobs": [{"id": 1}]},
                    {"total_count": 2, "jobs": [{"id": 2}]},
                ]
                if mode == "truncated":
                    pages[1]["jobs"] = []
                elif mode == "count_changed":
                    pages[1]["total_count"] = 1
                elif mode == "duplicate":
                    pages[1]["jobs"][0]["id"] = 1
                else:
                    pages[0]["total_count"] = "2"
                with patch.object(self.api, "get", side_effect=pages):
                    with self.assertRaises(GATE.GateError):
                        self.api.items("/jobs", "jobs")

    def test_api_errors_and_malformed_json_fail_closed(self):
        client = GATE.GitHub()
        for failure in (HTTPError("url", 403, "rate limit", {}, None),
                        HTTPError("url", 500, "unavailable", {}, None), URLError("offline")):
            with patch.object(client.opener, "open", side_effect=failure):
                with self.assertRaises(GATE.GateError):
                    client.get("/actions/runs")
        for payload in (b"not json", b"[]"):
            with patch.object(client.opener, "open", return_value=io.BytesIO(payload)):
                with self.assertRaises(GATE.GateError):
                    client.get("/actions/runs")

    def test_malformed_response_cli_returns_failure(self):
        with patch.object(GATE.GitHub, "get", return_value={}), \
                patch("sys.argv", ["check-release-ci.py", TAG, SHA]), \
                patch("sys.stderr", new_callable=io.StringIO):
            self.assertEqual(GATE.main(), 1)

    def test_token_is_optional_and_only_sent_to_canonical_api(self):
        for token in ("", "test-token"):
            client = GATE.GitHub(token)
            with patch.object(client.opener, "open", return_value=io.BytesIO(b"{}")) as opened:
                client.get("/actions/runs", page=1)
            request = opened.call_args.args[0]
            self.assertEqual(request.full_url, GATE.API + "/actions/runs?page=1")
            self.assertEqual(request.get_header("Authorization"), "Bearer test-token" if token else None)


class ReleaseDriverTests(unittest.TestCase):
    def test_denied_gate_stops_both_registries_before_any_tools_or_login(self):
        # Stub Git and Python so this tests the actual shell driver's ordering
        # without creating tags, contacting APIs, or using registry credentials.
        for registry in ("ghcr.io", "git.lucas.icu"):
            with self.subTest(registry=registry), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                calls = directory / "calls"
                programs = {
                    "git": f'case "$1" in rev-parse) echo {SHA} ;; merge-base) exit 0 ;; *) exit 99 ;; esac\n',
                    "python3": 'printf "%s\\n" "$*" >> "$TEST_CALLS"\nexit 42\n',
                }
                for name in ("curl", "docker", "helm", "go", "awk"):
                    programs[name] = f'echo unexpected-{name} >> "$TEST_CALLS"\nexit 99\n'
                for name, body in programs.items():
                    path = directory / name
                    path.write_text("#!/bin/sh\n" + body)
                    path.chmod(0o755)
                env = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ["PATH"],
                           TEST_CALLS=str(calls), TAG=TAG, REGISTRY_HOST=registry,
                           REGISTRY_USER="unused", REGISTRY_TOKEN="unused")
                result = subprocess.run(["bash", str(ROOT / "hack/release.sh")], cwd=ROOT,
                                        env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 42, result.stderr)
                self.assertEqual(calls.read_text(), f"hack/check-release-ci.py {TAG} {SHA}\n")

    @staticmethod
    def workflow_step(workflow, name):
        match = re.search(r"^      - name: " + re.escape(name) + r"\n(.*?)(?=^      - |\Z)",
                          workflow, re.MULTILINE | re.DOTALL)
        if match is None:
            raise AssertionError(f"Missing workflow step: {name}")
        return match.group(1)

    def test_old_tag_drivers_cannot_bypass_workflow_policy(self):
        workflows = (
            (".github/workflows/release.yml", "GHCR artifacts", "github.workflow_sha"),
            (".forgejo/workflows/release.yml", "Forgejo packages", "github.sha"),
        )
        for path, destination, policy_ref in workflows:
            workflow = (ROOT / path).read_text()
            # Exercise the actual entry-point commands, not the tagged driver:
            # old tags may omit the gate entirely or contain an obsolete policy.
            policy_checkout = self.workflow_step(workflow, "Check out trusted release policy")
            self.assertIn("ref: ${{ " + policy_ref + " }}", policy_checkout)
            self.assertNotIn("inputs.tag", policy_checkout)
            self.assertIn("github.ref == 'refs/heads/main'", workflow)
            preserve = self.workflow_step(workflow, "Preserve trusted release policy")
            publish = self.workflow_step(workflow, "Verify canonical CI, build and publish " + destination)
            self.assertNotIn("continue-on-error", workflow)
            self.assertNotIn("if:", publish)
            self.assertLess(workflow.index("Preserve trusted release policy"),
                            workflow.index("ref: ${{ github.event_name"))
            preserve_script = textwrap.dedent(preserve.split("run: |\n", 1)[1])
            publish_script = textwrap.dedent(publish.split("run: |\n", 1)[1])
            for outcome in (0, 42):
                for old_policy in (False, True):
                    with self.subTest(workflow=path, outcome=outcome, old_policy=old_policy), \
                            tempfile.TemporaryDirectory() as temporary:
                        directory = Path(temporary)
                        (directory / "hack").mkdir()
                        (directory / "bin").mkdir()
                        (directory / "temp").mkdir()
                        calls = directory / "calls"
                        policy = directory / "hack/check-release-ci.py"
                        policy.write_text(
                            "import os, sys\nfrom pathlib import Path\n"
                            f"assert sys.argv[1:] == [{TAG!r}, {SHA!r}]\n"
                            "Path(os.environ['TEST_CALLS']).write_text('trusted-policy\\n')\n"
                            "sys.exit(int(os.environ['TEST_GATE_RESULT']))\n")
                        git = directory / "bin/git"
                        git.write_text(f"#!/bin/sh\ntest \"$1\" = rev-parse || exit 99\necho {SHA}\n")
                        git.chmod(0o755)
                        env = dict(os.environ, PATH=str(directory / "bin") + os.pathsep + os.environ["PATH"],
                                   RUNNER_TEMP=str(directory / "temp"), TAG=TAG,
                                   TEST_CALLS=str(calls), TEST_GATE_RESULT=str(outcome))
                        subprocess.run(["bash", "-eu", "-c", preserve_script], cwd=directory,
                                       env=env, check=True, capture_output=True)
                        # Simulate the second checkout replacing the source tree
                        # with an old tag, while runner.temp retains current policy.
                        if old_policy:
                            policy.write_text("raise SystemExit(0)\n")
                        else:
                            policy.unlink()
                        (directory / "hack/release.sh").write_text('echo tagged-driver >> "$TEST_CALLS"\n')
                        result = subprocess.run(["bash", "-eu", "-c", publish_script], cwd=directory,
                                                env=env, capture_output=True, text=True)
                        self.assertEqual(result.returncode, outcome, result.stderr)
                        expected = "trusted-policy\n" + ("tagged-driver\n" if outcome == 0 else "")
                        self.assertEqual(calls.read_text(), expected)


if __name__ == "__main__":
    unittest.main()
