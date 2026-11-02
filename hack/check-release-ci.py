#!/usr/bin/env python3
"""Fail closed unless canonical GitHub CI passed for a release tag's commit."""
import argparse
import json
import os
import re
import sys
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import HTTPRedirectHandler, Request, build_opener


REPOSITORY = "lictl/forgejo-operator"
API = "https://api.github.com/repos/" + REPOSITORY
# Update these alongside workflow/job renames. All additional jobs must pass too.
# The two E2E jobs are mandatory, including when the older single-job CI passed.
REQUIRED = {
    ".github/workflows/test.yml": {"Run on Ubuntu", "Prometheus rules"},
    ".github/workflows/lint.yml": {"Run on Ubuntu"},
    ".github/workflows/chart.yml": {"chart"},
    ".github/workflows/test-e2e.yml": {"Kubernetes 1.30", "Kubernetes 1.37"},
    ".github/workflows/govulncheck.yml": {"govulncheck"},
}


class GateError(Exception):
    """The available evidence does not authorize publication."""


def require(condition, message):
    if not condition:
        raise GateError(message)


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class GitHub:
    def __init__(self, token=""):
        self.headers = {
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "forgejo-operator-release-gate",
        }
        if token:
            self.headers["Authorization"] = "Bearer " + token
        self.opener = build_opener(NoRedirect())

    def get(self, path, **query):
        url = API + path + ("?" + urlencode(query) if query else "")
        try:
            with self.opener.open(Request(url, headers=self.headers), timeout=30) as response:
                result = json.load(response)
        except HTTPError as error:
            error.close()
            raise GateError(f"GitHub GET {path}: HTTP {error.code}; retry after resolving the API failure") from error
        except (URLError, OSError, ValueError) as error:
            raise GateError(f"GitHub GET {path}: unavailable or invalid response") from error
        require(isinstance(result, dict), f"GitHub GET {path}: expected an object")
        return result

    def items(self, path, key, **query):
        # Count and page consistency checks detect truncation, including GitHub's
        # 1,000-result cap on filtered run searches. Never authorize on a partial list.
        items = []
        total = None
        for page in range(1, 101):
            result = self.get(path, **query, per_page=100, page=page)
            count, batch = result.get("total_count"), result.get(key)
            require(type(count) is int and count >= 0 and isinstance(batch, list),
                    f"Malformed {key} page")
            require(total is None or total == count, f"{key} changed during pagination; retry")
            total = count
            require(len(batch) <= 100 and len(items) + len(batch) <= total,
                    f"Inconsistent {key} count")
            items.extend(batch)
            if len(items) == total:
                ids = [item["id"] for item in items]
                require(len(set(ids)) == len(ids), f"Duplicate {key} across pages; retry")
                return items
            require(batch, f"Truncated {key}; retry")
        raise GateError(f"Too many {key} pages; cannot verify complete results")


def canonical_source(api, tag, revision):
    obj = api.get("/git/ref/tags/" + tag)["object"]
    for _ in range(16):
        require(re.fullmatch(r"[0-9a-f]{40}", obj["sha"]), "Invalid tag object SHA")
        if obj["type"] == "commit":
            break
        require(obj["type"] == "tag", "Release tag does not point to a commit")
        obj = api.get("/git/tags/" + obj["sha"])["object"]
    else:
        raise GateError("Too many nested annotated tags")
    require(obj["sha"] == revision, "Canonical GitHub tag differs from the checked-out commit")
    main = api.get("/git/ref/heads/main")["object"]
    require(main["type"] == "commit" and re.fullmatch(r"[0-9a-f]{40}", main["sha"]),
            "Invalid canonical main ref")
    comparison = api.get("/compare/" + revision + "..." + main["sha"], per_page=1)
    require(comparison["status"] in ("ahead", "identical") and
            comparison["merge_base_commit"]["sha"] == revision,
            "Release commit is not in canonical GitHub main history")


def latest_runs(api, revision):
    selected = {}
    for run in api.items("/actions/runs", "workflow_runs", head_sha=revision, branch="main"):
        path = run["path"]
        if path not in REQUIRED:
            continue
        # PR merge tests and tag pushes cannot stand in for main-branch CI.
        if run["head_branch"] != "main" or run["event"] not in ("push", "schedule", "workflow_dispatch"):
            continue
        require(run["head_sha"] == revision, f"{path}: API returned a different commit")
        require(type(run["run_number"]) is int and type(run["id"]) is int,
                f"{path}: invalid run identity")
        if path not in selected or (run["run_number"], run["id"]) > (selected[path]["run_number"], selected[path]["id"]):
            selected[path] = run
    require(set(selected) == set(REQUIRED),
            "Missing canonical main CI: " + ", ".join(sorted(set(REQUIRED) - set(selected))))
    return selected


def successful_run(run, path, revision):
    require(run["path"] == path and run["head_sha"] == revision and
            run["head_branch"] == "main" and
            run["repository"]["full_name"] == REPOSITORY and
            run["event"] in ("push", "schedule", "workflow_dispatch"),
            f"{path}: unexpected run source")
    require(type(run["run_attempt"]) is int and run["run_attempt"] >= 1,
            f"{path}: missing run attempt")
    require(run["status"] == "completed" and run["conclusion"] == "success",
            f"{path}: run {run['id']} attempt {run['run_attempt']} is not completed successfully")


def check(api, tag, revision):
    require(re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?", tag), "Invalid version tag")
    require(re.fullmatch(r"[0-9a-f]{40}", revision), "Expected a full checked-out commit SHA")
    canonical_source(api, tag, revision)
    selected = latest_runs(api, revision)
    verified = {}
    for path, listed in selected.items():
        successful_run(listed, path, revision)
        run = api.get(f"/actions/runs/{listed['id']}")
        successful_run(run, path, revision)
        require(run["id"] == listed["id"] and run["run_attempt"] == listed["run_attempt"],
                f"{path}: run identity or attempt changed; retry")
        attempt = run["run_attempt"]
        # The attempt-specific endpoint prevents successful jobs from an old
        # attempt masking skipped/missing jobs. Partial reruns must be rerun in full.
        jobs = api.items(f"/actions/runs/{run['id']}/attempts/{attempt}/jobs", "jobs")
        names = set()
        for job in jobs:
            require(job["run_id"] == run["id"] and job["head_sha"] == revision and
                    job["run_attempt"] == attempt,
                    f"{path}: job belongs to a different run, attempt or commit")
            require(job["name"] not in names, f"{path}: duplicate job name")
            names.add(job["name"])
            require(job["status"] == "completed" and job["conclusion"] == "success",
                    f"{path}: job {job['name']} is not completed successfully")
        require(REQUIRED[path] <= names,
                f"{path}: latest attempt lacks required jobs; rerun all jobs: " +
                ", ".join(sorted(REQUIRED[path] - names)))
        current = api.get(f"/actions/runs/{run['id']}")
        successful_run(current, path, revision)
        require(current["id"] == run["id"] and current["run_attempt"] == attempt,
                f"{path}: attempt changed while checking jobs; retry")
        verified[path] = (run["id"], attempt)
    # Detect newly queued independent runs or reruns while jobs were being read.
    for path, run in latest_runs(api, revision).items():
        successful_run(run, path, revision)
        require((run["id"], run["run_attempt"]) == verified[path],
                f"{path}: CI changed while checking; retry")
    return verified


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("revision", help="full SHA of the checked-out tag commit")
    args = parser.parse_args()
    try:
        verified = check(GitHub(os.environ.get("CI_GITHUB_TOKEN", "")), args.tag, args.revision)
    except (GateError, KeyError, TypeError, ValueError) as error:
        print(f"Release CI gate refused publication: {error}", file=sys.stderr)
        return 1
    print(f"Canonical GitHub CI passed for {args.tag} ({args.revision}):")
    for path, (run, attempt) in verified.items():
        print(f"  {path}: https://github.com/{REPOSITORY}/actions/runs/{run}/attempts/{attempt}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
