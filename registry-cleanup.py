"""Audit/purge GHCR manifests built from explicitly selected Git revisions.

Run through the manual registry-cleanup workflow. Tokens and revisions are
provided in environment variables; no application credentials are needed.
"""
import base64
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, newurl):
        result = super().redirect_request(request, file, code, message, headers, newurl)
        if result and urllib.parse.urlparse(request.full_url).hostname != urllib.parse.urlparse(newurl).hostname:
            result.remove_header("Authorization")
        return result


opener = urllib.request.build_opener(SafeRedirect())


def request(url, headers, method="GET"):
    for attempt in range(4):
        try:
            with opener.open(urllib.request.Request(url, headers=headers, method=method), timeout=30) as response:
                body = response.read()
                return json.loads(body) if body else None
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            if error.code in (429, 500, 502, 503, 504) and attempt < 3:
                time.sleep(2 ** attempt)
                continue
            try:
                detail = json.loads(error.read()).get("message", "request failed")
            except Exception:
                detail = "request failed"
            raise RuntimeError(f"HTTP {error.code} for {method} {urllib.parse.urlparse(url).path}: {detail}") from None


def main():
    mode = os.environ.get("CLEANUP_MODE", "AUDIT")
    if mode not in {"AUDIT", "PURGE"}:
        raise ValueError("Mode must be AUDIT or PURGE")
    affected = set(os.environ["AFFECTED_REVISIONS"].split())
    if not affected or len(affected) > 100 or not all(re.fullmatch(r"[a-f0-9]{40}", rev) for rev in affected):
        raise ValueError("Supply 1..100 exact lowercase Git commit SHAs")
    audited_digests = set(os.environ.get("AUDITED_MANIFEST_DIGESTS", "").split())
    if len(audited_digests) > 200 or not all(re.fullmatch(r"sha256:[a-f0-9]{64}", digest) for digest in audited_digests):
        raise ValueError("Previously audited manifests must be exact sha256 digests")
    owner, package = os.environ["GITHUB_REPOSITORY"].split("/", 1)
    image = owner.lower() + "/" + package.lower()
    scope = "orgs" if os.environ["PACKAGE_OWNER_TYPE"] == "Organization" else "users"
    api_url = f"https://api.github.com/{scope}/{owner}/packages/container/{urllib.parse.quote(package.lower(), safe='')}"
    api_headers = {
        "Authorization": "Bearer " + os.environ["GH_TOKEN"],
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "imgserver-registry-cleanup",
    }
    credentials = base64.b64encode((os.environ["GITHUB_ACTOR"] + ":" + os.environ["GH_TOKEN"]).encode()).decode()
    token_url = "https://ghcr.io/token?" + urllib.parse.urlencode({"service": "ghcr.io", "scope": "repository:" + image + ":pull"})
    token = request(token_url, {"Authorization": "Basic " + credentials})["token"]
    registry_headers = {
        "Authorization": "Bearer " + token,
        "Accept": ", ".join([
            "application/vnd.oci.image.index.v1+json",
            "application/vnd.docker.distribution.manifest.list.v2+json",
            "application/vnd.oci.image.manifest.v1+json",
            "application/vnd.docker.distribution.manifest.v2+json",
        ]),
    }
    versions = []
    page = 1
    while True:
        batch = request(api_url + f"/versions?per_page=100&page={page}", api_headers)
        if not batch:
            break
        versions.extend(batch)
        page += 1
    manifests, configs = {}, {}

    def manifest(digest):
        if digest not in manifests:
            manifests[digest] = request(f"https://ghcr.io/v2/{image}/manifests/{digest}", registry_headers)
        return manifests[digest]

    def revision(item):
        digest = item.get("config", {}).get("digest")
        if not digest:
            return None
        if digest not in configs:
            configs[digest] = request(f"https://ghcr.io/v2/{image}/blobs/{digest}", registry_headers) or {}
        return ((configs[digest].get("config") or {}).get("Labels") or {}).get("org.opencontainers.image.revision")

    # Include children even if GitHub only lists their parent index as a version.
    pending = [v["name"] for v in versions if re.fullmatch(r"sha256:[a-f0-9]{64}", v["name"])]
    inspected = set()
    # Explicit audited digests allow a resumed purge to remove detached
    # attestations after their source image index has already been deleted.
    tainted = set(audited_digests)
    while pending:
        digest = pending.pop()
        if digest in inspected:
            continue
        inspected.add(digest)
        item = manifest(digest)
        if item is None:
            continue
        pending.extend(child["digest"] for child in item.get("manifests", []))
        if revision(item) in affected:
            tainted.add(digest)
    changed = True
    while changed:
        before = len(tainted)
        for digest, item in manifests.items():
            if item is None:
                continue
            children = item.get("manifests", [])
            if any(child["digest"] in tainted for child in children):
                tainted.add(digest)
            for child in children:
                if child.get("annotations", {}).get("vnd.docker.reference.digest") in tainted:
                    tainted.add(child["digest"])
        changed = len(tainted) != before
    candidates = [v for v in versions if v["name"] in tainted]
    candidates.sort(key=lambda v: (not bool((manifests.get(v["name"]) or {}).get("manifests")), v["id"]))
    report = {
        "mode": mode, "image": "ghcr.io/" + image, "inspected_versions": len(versions),
        "matching_versions": [{"id": v["id"], "digest": v["name"], "tags": v.get("metadata", {}).get("container", {}).get("tags", [])} for v in candidates],
        "deleted_ids": [],
    }
    print("Matched versions:", len(candidates), flush=True)
    if mode == "PURGE":
        for version in candidates:
            request(api_url + "/versions/" + str(version["id"]), api_headers, method="DELETE")
            report["deleted_ids"].append(version["id"])
            print("Deleted version", version["id"], version["name"], flush=True)
        remaining = []
        for digest in tainted:
            url = f"https://ghcr.io/v2/{image}/manifests/{digest}"
            if request(url, registry_headers) is not None:
                remaining.append(digest)
        report["remaining_manifest_digests"] = remaining
    print("PRIVACY_CLEANUP_REPORT=" + json.dumps(report), flush=True)
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
        summary.write(f"GHCR cleanup: {mode}. Matched {len(candidates)} versions; deleted {len(report['deleted_ids'])}.\n")
    if report.get("remaining_manifest_digests"):
        raise RuntimeError("Some old manifests remain accessible after version deletion; provider cleanup may be required")


if __name__ == "__main__":
    main()
