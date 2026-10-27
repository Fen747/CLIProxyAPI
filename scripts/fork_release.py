#!/usr/bin/env python3
"""Stage verified releases and switch a systemd user service after drainage."""

import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


MAX_BINARY_BYTES = 256 * 1024 * 1024


def digest(stream):
    value = hashlib.sha256()
    for block in iter(lambda: stream.read(1024 * 1024), b""):
        value.update(block)
    return value.hexdigest()


def validate_manifest(value):
    if not isinstance(value, dict) or value.get("schema") != 1:
        raise ValueError("Unsupported release manifest")
    release = value.get("release", "")
    if not isinstance(release, str) or not re.fullmatch(r"(?:fleet|upstream)-v[0-9][A-Za-z0-9.+-]{0,100}", release):
        raise ValueError("Invalid release identifier")
    version = value.get("version")
    if not isinstance(version, str) or release.split("-v", 1)[1] != version:
        raise ValueError("Release identifier and version differ")
    if value.get("platform") != "linux-amd64":
        raise ValueError("Expected a linux-amd64 release")
    if "maintenance_protocol" in value and (type(value["maintenance_protocol"]) is not int or value["maintenance_protocol"] != 1):
        raise ValueError("Unsupported maintenance protocol")
    for field, pattern in (("commit", r"[a-f0-9]{40}"), ("binary_sha256", r"[a-f0-9]{64}")):
        if not isinstance(value.get(field), str) or not re.fullmatch(pattern, value[field]):
            raise ValueError("Invalid " + field)
    return value


def verify_binary(stream, expected):
    header = stream.read(64)
    if len(header) != 64 or header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
        raise ValueError("Expected a Linux x86-64 ELF executable")
    stream.seek(0)
    if digest(stream) != expected:
        raise ValueError("Binary checksum mismatch")


class Releases:
    def __init__(self, root):
        self.root = Path(root).expanduser().absolute()

    @contextmanager
    def transaction(self, apply):
        if not apply:
            yield
            return
        if self.root.is_symlink():
            raise ValueError("Release root must not be a symlink")
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        if self.root.stat().st_uid != os.getuid() or self.root.stat().st_mode & 0o022:
            raise ValueError("Release root must be owned by this user and not writable by others")
        releases = self.root / "releases"
        if releases.is_symlink():
            raise ValueError("Release directory must not be a symlink")
        releases.mkdir(exist_ok=True, mode=0o700)
        if releases.stat().st_uid != os.getuid() or releases.stat().st_mode & 0o022:
            raise ValueError("Release directory must be owned by this user and not writable by others")
        fd = os.open(self.root / ".lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise ValueError("Another release operation is running") from error
            yield

    def installed(self, release):
        if not re.fullmatch(r"(?:fleet|upstream)-v[0-9][A-Za-z0-9.+-]{0,100}", release):
            raise ValueError("Invalid release identifier")
        directory = self.root / "releases" / release
        if directory.is_symlink() or (self.root / "releases").is_symlink():
            raise ValueError("Installed release must not be a symlink")
        if directory.stat().st_uid != os.getuid() or directory.stat().st_mode & 0o022:
            raise ValueError("Installed release must be owned by this user and not writable by others")
        for name in ("manifest.json", "cli-proxy-api"):
            if (directory / name).is_symlink():
                raise ValueError("Installed release files must not be symlinks")
            if (directory / name).stat().st_uid != os.getuid() or (directory / name).stat().st_mode & 0o022:
                raise ValueError("Installed files must be owned by this user and not writable by others")
        manifest = validate_manifest(json.loads((directory / "manifest.json").read_text()))
        if manifest["release"] != release:
            raise ValueError("Installed release identifier mismatch")
        with (directory / "cli-proxy-api").open("rb") as binary:
            verify_binary(binary, manifest["binary_sha256"])
        return manifest

    def target(self, name):
        link = self.root / name
        if not link.is_symlink():
            if link.exists():
                raise ValueError(name + " must be a managed symlink")
            return None
        target = link.readlink()
        if target.parent != Path("releases"):
            raise ValueError("Release link must point into releases/")
        self.installed(target.name)
        return target.name

    def link(self, name, release):
        destination = self.root / name
        if destination.is_symlink():
            if destination.readlink().parent != Path("releases"):
                raise ValueError("Refusing to replace an unmanaged release link")
        elif destination.exists():
            raise ValueError("Refusing to replace an unmanaged release path")
        temporary = self.root / ("." + name + "-" + uuid.uuid4().hex)
        try:
            temporary.symlink_to(Path("releases") / release)
            os.replace(temporary, destination)
        finally:
            temporary.unlink(missing_ok=True)

    def install(self, manifest, source, license_data=None):
        destination = self.root / "releases" / manifest["release"]
        if destination.exists() or destination.is_symlink():
            if self.installed(manifest["release"]) != manifest:
                raise ValueError("An immutable release with different contents already exists")
            if license_data is not None and (destination / "LICENSE").read_bytes() != license_data:
                raise ValueError("An immutable release with different license contents already exists")
            return False
        with tempfile.TemporaryDirectory(prefix=".stage-", dir=self.root) as staging:
            directory = Path(staging)
            binary = directory / "cli-proxy-api"
            with binary.open("wb") as output:
                shutil.copyfileobj(source, output, 1024 * 1024)
            with binary.open("rb") as installed:
                verify_binary(installed, manifest["binary_sha256"])
            binary.chmod(0o755)
            metadata = directory / "manifest.json"
            metadata.write_text(json.dumps(manifest, sort_keys=True) + "\n")
            metadata.chmod(0o600)
            if license_data is not None:
                notice = directory / "LICENSE"
                notice.write_bytes(license_data)
                notice.chmod(0o644)
            os.rename(directory, destination)
        return True

    def stage(self, archive, checksum, apply=False):
        if not re.fullmatch(r"[a-f0-9]{64}", checksum):
            raise ValueError("Expected an explicit SHA256 checksum")
        with self.transaction(apply), Path(archive).open("rb") as source:
            if digest(source) != checksum:
                raise ValueError("Archive checksum mismatch")
            source.seek(0)
            with tarfile.open(fileobj=source, mode="r:gz") as bundle:
                members = []
                for member in bundle:
                    members.append(member)
                    if len(members) > 3:
                        raise ValueError("Archive must contain only regular manifest.json, LICENSE and cli-proxy-api files")
                if (len(members) != 3 or {m.name for m in members} != {"manifest.json", "LICENSE", "cli-proxy-api"}
                        or any(not m.isfile() for m in members)):
                    raise ValueError("Archive must contain only regular manifest.json, LICENSE and cli-proxy-api files")
                if (bundle.getmember("manifest.json").size > 65536 or bundle.getmember("LICENSE").size > 65536
                        or bundle.getmember("cli-proxy-api").size > MAX_BINARY_BYTES):
                    raise ValueError("Release files exceed size limits")
                with bundle.extractfile("manifest.json") as metadata:
                    manifest = validate_manifest(json.load(metadata))
                with bundle.extractfile("LICENSE") as notice:
                    license_data = notice.read()
                with bundle.extractfile("cli-proxy-api") as binary:
                    verify_binary(binary, manifest["binary_sha256"])
                    binary.seek(0)
                    changed = self.install(manifest, binary, license_data) if apply else None
        return {"action": "stage", "release": manifest["release"], "apply": apply, "changed": changed}

    def adopt(self, binary, checksum, version, commit, apply=False):
        manifest = validate_manifest({"schema": 1, "release": "upstream-v" + version,
                                      "version": version, "commit": commit, "platform": "linux-amd64",
                                      "binary_sha256": checksum})
        with self.transaction(apply), Path(binary).open("rb") as source:
            verify_binary(source, checksum)
            old = self.target("current")
            if old is not None and old != manifest["release"]:
                raise ValueError("A different current release is already adopted")
            if apply:
                source.seek(0)
                self.install(manifest, source)
                self.link("current", manifest["release"])
        return {"action": "adopt", "release": manifest["release"], "apply": apply}

    def activate(self, release, service, health, apply=False, maintenance=None):
        with self.transaction(apply):
            if release is None:
                release = self.target("previous")
            if release is None:
                raise ValueError("No previous release is available")
            manifest = self.installed(release)
            previous = self.target("current")
            if previous is None:
                raise ValueError("Adopt and verify the existing binary before activating a release")
            if maintenance is None and (self.root / ".maintenance-operation.json").exists():
                raise ValueError("Recover the saved maintenance operation with resume before switching releases")
            executable = self.root / "current" / "cli-proxy-api"
            if previous == release:
                if apply:
                    if maintenance is not None:
                        state = maintenance.inspect(service.require_running(executable))
                        if state["maintenance"]["draining"]:
                            raise ValueError("Release is already selected but admission is closed; recover with resume")
                    health(manifest)
                return {"action": "activate", "release": release, "apply": apply, "changed": False}
            old_previous = self.target("previous")
            if maintenance is None:
                service.require_stopped(executable)
            else:
                if manifest.get("maintenance_protocol") != 1:
                    raise ValueError("Target release does not declare durable maintenance support; no drainage started")
                pid = service.require_running(executable)
                maintenance.inspect(pid)
            if not apply:
                return {"action": "activate", "from": previous, "release": release, "apply": False}
            if maintenance is not None:
                health(self.installed(previous))
                maintenance.drain(pid)
                try:
                    if service.require_running(executable) != pid:
                        raise RuntimeError("Service process changed after drainage; refusing to stop")
                    service.stop()
                    service.require_stopped(executable)
                except BaseException:
                    maintenance.resume()
                    raise
            # Keep a recovery target even if this operation is killed after the switch.
            try:
                self.link("previous", previous)
                self.link("current", release)
                service.start()
                health(manifest)
            except BaseException as error:
                # Never repoint a release while its candidate process may still be running.
                if maintenance is not None:
                    try:
                        service.require_stopped(executable)
                    except ValueError:
                        # An unhealthy candidate may have admitted work. If its
                        # control API cannot prove it drained, leave it in place.
                        candidate_pid = service.require_running(executable)
                        maintenance.drain(candidate_pid, continue_operation=True)
                        if service.require_running(executable) != candidate_pid:
                            raise RuntimeError("Candidate process changed after drainage; refusing to stop")
                service.stop()
                service.require_stopped(executable)
                self.link("current", previous)
                if old_previous is not None:
                    self.link("previous", old_previous)
                service.start()
                health(self.installed(previous))
                if maintenance is not None:
                    maintenance.resume()
                raise RuntimeError("Candidate failed; previous release restored and healthy") from error
            if maintenance is not None:
                maintenance.resume()
        return {"action": "activate", "from": previous, "release": release, "apply": True, "changed": True}


class UserService:
    def __init__(self, unit):
        if not re.fullmatch(r"[A-Za-z0-9_.@-]+\.service", unit):
            raise ValueError("Expected a systemd user service name")
        if sys.platform != "linux" or platform.machine() not in ("x86_64", "amd64") or os.getuid() == 0:
            raise ValueError("Activation requires a non-root Linux x86-64 user")
        self.unit = unit

    def run(self, *args):
        result = subprocess.run(["systemctl", "--user", *args, self.unit], capture_output=True, text=True, timeout=30)
        if result.returncode:
            raise RuntimeError("systemd user service operation failed")
        return result.stdout

    def managed_properties(self, executable):
        properties = dict(line.split("=", 1) for line in self.run("show", "--property=LoadState,ActiveState,ExecStart,MainPID,ControlPID").splitlines() if "=" in line)
        match = re.search(r"(?:^|[ {])path=([^ ;]+)", properties.get("ExecStart", ""))
        if match is None or Path(match[1]) != executable:
            raise ValueError("Service ExecStart does not use the managed current/cli-proxy-api path")
        return properties

    def require_stopped(self, executable):
        properties = self.managed_properties(executable)
        if (properties.get("LoadState") != "loaded" or properties.get("ActiveState") not in ("inactive", "failed")
                or properties.get("MainPID") != "0" or properties.get("ControlPID") != "0"):
            raise ValueError("Drain and stop the service before activation; this tool will not stop live work")

    def require_running(self, executable):
        properties = self.managed_properties(executable)
        pid = properties.get("MainPID", "")
        if (properties.get("LoadState") != "loaded" or properties.get("ActiveState") != "active"
                or not pid.isdecimal() or int(pid) <= 0 or properties.get("ControlPID") != "0"):
            raise ValueError("Automatic drainage requires an active service with one stable main process")
        return int(pid)

    def start(self):
        self.run("reset-failed")
        self.run("start")

    def stop(self):
        self.run("stop")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def management_request(base_url, key_file):
    parsed = urllib.parse.urlsplit(base_url)
    if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "::1", "localhost")
            or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ("", "/")):
        raise ValueError("Health URL must be a loopback HTTP origin")
    path = Path(key_file).expanduser()
    if path.stat().st_uid != os.getuid() or path.stat().st_mode & 0o077:
        raise ValueError("Management key file must be private and owned by this user")
    key = path.read_text().strip()
    if not key or "\n" in key or "\r" in key:
        raise ValueError("Invalid management key file")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(path, method="GET", data=None):
        body = None if data is None else json.dumps(data).encode()
        headers = {"Authorization": "Bearer " + key, "Content-Type": "application/json"}
        request = urllib.request.Request(base_url.rstrip("/") + path, data=body, headers=headers, method=method)
        return opener.open(request, timeout=2)
    return request


def management_health(base_url, key_file, wait=10, allow_draining=False):
    request = management_request(base_url, key_file)

    def check(manifest):
        deadline = time.monotonic() + wait
        while True:
            try:
                with request("/v8/management/config") as response:
                    version = response.headers.get("X-CPA-VERSION", "")
                    commit = response.headers.get("X-CPA-COMMIT", "")
                    if version == manifest["version"] and len(commit) >= 7 and manifest["commit"].startswith(commit):
                        if manifest.get("maintenance_protocol") == 1 and not allow_draining:
                            state = Maintenance(request).state(require_supported=False)
                            if not state["maintenance"]["draining"]:
                                return
                        else:
                            return
            except urllib.error.HTTPError as error:
                error.close()
            except (OSError, urllib.error.URLError, RuntimeError):
                pass
            if time.monotonic() >= deadline:
                raise RuntimeError("Authenticated health/version or admission check failed")
            time.sleep(0.2)
    return check


class Maintenance:
    path = "/v8/management/server/maintenance"

    def __init__(self, request, wait=300, state_file=None):
        if not 0 <= wait <= 3600:
            raise ValueError("Drain wait must be between 0 and 3600 seconds")
        self.request = request
        self.wait = wait
        self.token = uuid.uuid4().hex
        self.instance = None
        self.pid = None
        self.state_file = state_file

    def state(self, method="GET", require_supported=True):
        try:
            with self.request(self.path, method, None if method == "GET" else {"token": self.token, "instance": self.instance}) as response:
                value = json.loads(response.read(4097))
        except urllib.error.HTTPError as error:
            error.close()
            raise RuntimeError("Maintenance control rejected; no service stop authorized") from None
        except (OSError, ValueError, urllib.error.URLError):
            raise RuntimeError("Maintenance control unavailable; no service stop authorized") from None
        state = value.get("maintenance") if isinstance(value, dict) else None
        if (not isinstance(value, dict) or type(value.get("supported")) is not bool or type(value.get("pid")) is not int or not isinstance(state, dict)
                or not isinstance(state.get("instance"), str) or not state["instance"]
                or type(state.get("durable")) is not bool
                or type(state.get("active")) is not int or state["active"] < 0
                or type(state.get("draining")) is not bool or type(state.get("ready")) is not bool):
            raise RuntimeError("Maintenance state invalid or unsupported")
        if require_supported and (not value["supported"] or not state["durable"]):
            raise RuntimeError("Maintenance state unsupported for automatic drainage")
        return value

    def inspect(self, pid):
        value = self.state()
        if value["pid"] != pid:
            raise RuntimeError("Maintenance endpoint does not belong to the managed service")
        return value

    def drain(self, pid, continue_operation=False):
        initial = self.inspect(pid)
        if initial["maintenance"]["draining"] and not continue_operation:
            raise RuntimeError("Proxy already in maintenance; resume the existing operation first")
        self.pid = pid
        self.instance = initial["maintenance"]["instance"]
        if self.state_file is not None and (not continue_operation or not self.state_file.exists()):
            fd = os.open(self.state_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, "w") as saved:
                json.dump({"token": self.token, "instance": self.instance, "pid": self.pid}, saved)
                saved.flush()
                os.fsync(saved.fileno())
        deadline = time.monotonic() + self.wait
        try:
            while True:
                value = self.state("PUT")
                state = value["maintenance"]
                if value["pid"] != self.pid or state["instance"] != self.instance or not state["draining"]:
                    raise RuntimeError("Proxy instance changed during drainage; no service stop authorized")
                if state["ready"] and state["active"] == 0:
                    return
                if time.monotonic() >= deadline:
                    raise RuntimeError("Drain wait expired; active work was not interrupted")
                time.sleep(min(0.2, max(0, deadline - time.monotonic())))
        except BaseException:
            self.resume()
            raise

    def resume(self):
        # The same private token can reopen its durable drain after a restart;
        # the server rejects it if another operation now owns admission.
        try:
            value = self.state()
            if not value["maintenance"]["draining"]:
                self.clear()
                return
            self.instance = value["maintenance"]["instance"]
            self.state("DELETE")
            self.clear()
        except (RuntimeError, OSError):
            print("Could not resume admission automatically; saved operation retained for the resume command.", file=sys.stderr)
            raise RuntimeError("Maintenance recovery required; no further service change authorized") from None

    def clear(self):
        if self.state_file is not None:
            self.state_file.unlink(missing_ok=True)

    def load(self):
        if self.state_file.is_symlink() or self.state_file.stat().st_uid != os.getuid() or self.state_file.stat().st_mode & 0o077:
            raise ValueError("Maintenance operation file must be private and owned by this user")
        value = json.loads(self.state_file.read_text())
        if (not isinstance(value, dict) or not re.fullmatch(r"[a-f0-9]{32}", str(value.get("token", "")))
                or not isinstance(value.get("instance"), str) or type(value.get("pid")) is not int):
            raise ValueError("Invalid saved maintenance operation")
        self.token, self.instance, self.pid = value["token"], value["instance"], value["pid"]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default="~/.local/opt/cliproxy-fleet")
    subcommands = parser.add_subparsers(dest="command", required=True)
    stage = subcommands.add_parser("stage", help="Verify an archive; --apply installs it without activating it")
    stage.add_argument("--archive", required=True)
    stage.add_argument("--sha256", required=True)
    adopt = subcommands.add_parser("adopt", help="Copy the existing binary as the first rollback target; never changes its service")
    adopt.add_argument("--binary", required=True)
    adopt.add_argument("--sha256", required=True)
    adopt.add_argument("--version", required=True)
    adopt.add_argument("--commit", required=True)
    for name in ("activate", "rollback", "resume"):
        help_text = "Resume a saved drain on the running service" if name == "resume" else "Switch a stopped service, or use --drain for a running maintenance-aware proxy"
        operation = subcommands.add_parser(name, help=help_text)
        if name == "activate":
            operation.add_argument("--release", required=True)
        operation.add_argument("--service", required=True)
        operation.add_argument("--url", default="http://127.0.0.1:18317")
        operation.add_argument("--management-key-file", required=True)
        if name != "resume":
            operation.add_argument("--drain", action="store_true", help="Drain and stop a running maintenance-aware proxy before switching")
            operation.add_argument("--drain-wait", type=float, default=300, help="Maximum drainage wait in seconds (0-3600)")
    for operation in subcommands.choices.values():
        operation.add_argument("--apply", action="store_true", help="Apply this operation; default is a read-only plan")
    args = parser.parse_args(argv)
    releases = Releases(args.root)
    if args.command == "stage":
        result = releases.stage(args.archive, args.sha256, args.apply)
    elif args.command == "adopt":
        result = releases.adopt(args.binary, args.sha256, args.version, args.commit, args.apply)
    elif args.command == "resume":
        with releases.transaction(args.apply):
            maintenance = Maintenance(management_request(args.url, args.management_key_file), state_file=releases.root / ".maintenance-operation.json")
            maintenance.load()
            pid = UserService(args.service).require_running(releases.root / "current" / "cli-proxy-api")
            maintenance.inspect(pid)
            if args.apply:
                maintenance.resume()
            result = {"action": "resume", "apply": args.apply}
    else:
        release = args.release if args.command == "activate" else None
        maintenance = Maintenance(management_request(args.url, args.management_key_file), args.drain_wait,
                                  releases.root / ".maintenance-operation.json") if args.drain else None
        result = releases.activate(release, UserService(args.service), management_health(args.url, args.management_key_file, allow_draining=args.drain), args.apply, maintenance)
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    def interrupted(signum, frame):
        raise KeyboardInterrupt()

    signal.signal(signal.SIGTERM, interrupted)
    try:
        main()
    except KeyboardInterrupt:
        print("Release operation interrupted.", file=sys.stderr)
        sys.exit(130)
    except (OSError, json.JSONDecodeError, tarfile.TarError, subprocess.TimeoutExpired):
        print("Release operation failed; inspect local inputs and service state.", file=sys.stderr)
        sys.exit(1)
    except (ValueError, RuntimeError) as error:
        # These errors contain only fixed operational messages, never service stderr or config bodies.
        print("Release operation failed: " + str(error), file=sys.stderr)
        sys.exit(1)
