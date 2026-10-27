import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import threading
import unittest
from unittest.mock import patch

from fork_release import Maintenance, Releases, UserService, management_health


def executable(label):
    header = bytearray(64)
    header[:6] = b"\x7fELF\x02\x01"
    header[18:20] = b"\x3e\x00"
    return bytes(header) + label.encode()


class FakeService:
    def __init__(self, running=False):
        self.running = running
        self.events = []

    def require_stopped(self, path):
        if self.running:
            raise ValueError("Service is active")

    def require_running(self, path):
        if not self.running:
            raise ValueError("Service is stopped")
        return 123

    def start(self):
        self.events.append("start")
        self.running = True

    def stop(self):
        self.events.append("stop")
        self.running = False


class FakeMaintenanceAPI:
    def __init__(self, active=0):
        self.active = active
        self.draining = False
        self.instance = "original"
        self.pid = 123
        self.supported = True
        self.durable = True
        self.fail_resume = False
        self.change_instance_on_put = False
        self.methods = []

    def request(self, path, method="GET", data=None):
        self.methods.append(method)
        if method == "PUT":
            self.draining = True
            if self.change_instance_on_put:
                self.instance = "restarted"
        elif method == "DELETE":
            if self.fail_resume:
                raise OSError("simulated control outage")
            self.draining = False
        return io.BytesIO(json.dumps({"pid": self.pid, "supported": self.supported, "maintenance": {
            "instance": self.instance, "draining": self.draining, "active": self.active,
            "ready": self.draining and self.active == 0,
            "durable": self.durable,
        }}).encode())


class MaintenanceTests(unittest.TestCase):
    def test_invalid_or_unsupported_state_never_authorizes_stop(self):
        for body in (b"[]", b"{}", b"null", b"invalid", b'{"maintenance":false}'):
            with self.subTest(body=body):
                maintenance = Maintenance(lambda *args: io.BytesIO(body), wait=0)
                with self.assertRaisesRegex(RuntimeError, "invalid|unavailable"):
                    maintenance.drain(123)
        api = FakeMaintenanceAPI()
        api.supported = False
        with self.assertRaisesRegex(RuntimeError, "unsupported"):
            Maintenance(api.request).drain(123)
        self.assertEqual(api.methods, ["GET"])

    def test_wrong_pid_or_existing_maintenance_is_read_only(self):
        for wrong_pid in (True, False):
            api = FakeMaintenanceAPI()
            api.pid = 456 if wrong_pid else 123
            api.draining = not wrong_pid
            with self.assertRaises(RuntimeError):
                Maintenance(api.request).drain(123)
            self.assertEqual(api.methods, ["GET"])

    def test_changed_instance_resumes_only_the_original_owner_token(self):
        api = FakeMaintenanceAPI()
        api.change_instance_on_put = True
        with self.assertRaisesRegex(RuntimeError, "instance changed"):
            Maintenance(api.request, wait=0).drain(123)
        self.assertIn("DELETE", api.methods)

    def test_interrupted_wait_resumes_without_cancelling_work(self):
        api = FakeMaintenanceAPI(active=1)
        with patch("fork_release.time.sleep", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                Maintenance(api.request).drain(123)
        self.assertFalse(api.draining)
        self.assertEqual(api.active, 1)

    def test_failed_resume_keeps_private_record_for_recovery(self):
        api = FakeMaintenanceAPI(active=1)
        api.fail_resume = True
        with tempfile.TemporaryDirectory() as directory:
            saved = Path(directory) / "operation.json"
            operation = Maintenance(api.request, wait=0, state_file=saved)
            with self.assertRaisesRegex(RuntimeError, "recovery required"):
                operation.drain(123)
            self.assertEqual(saved.stat().st_mode & 0o777, 0o600)
            recovery = Maintenance(api.request, state_file=saved)
            recovery.load()
            self.assertEqual(recovery.token, operation.token)
            api.fail_resume = False
            recovery.resume()
            self.assertFalse(api.draining)
            self.assertFalse(saved.exists())


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.manager = Releases(self.directory / "managed")

    def archive(self, version="8.0.29-fleet.1", binary=None, extra=None, maintenance=False):
        binary = executable(version) if binary is None else binary
        manifest = {"schema": 1, "release": "fleet-v" + version, "version": version,
                    "commit": "a" * 40, "platform": "linux-amd64",
                    "binary_sha256": hashlib.sha256(binary).hexdigest()}
        if maintenance:
            manifest["maintenance_protocol"] = 1
        archive = self.directory / (version + ".tar.gz")
        with tarfile.open(archive, "w:gz") as bundle:
            for name, data in (("manifest.json", json.dumps(manifest).encode()), ("LICENSE", b"Test license notice\n"), ("cli-proxy-api", binary)):
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                bundle.addfile(entry, io.BytesIO(data))
            if extra:
                bundle.addfile(extra, io.BytesIO(b""))
        return archive, hashlib.sha256(archive.read_bytes()).hexdigest(), manifest

    def adopted(self):
        binary = self.directory / "existing-binary"
        binary.write_bytes(executable("original"))
        checksum = hashlib.sha256(binary.read_bytes()).hexdigest()
        self.manager.adopt(binary, checksum, "8.0.29", "b" * 40, apply=True)
        return "upstream-v8.0.29"

    def staged(self, version="8.0.29-fleet.1"):
        archive, checksum, manifest = self.archive(version, maintenance=True)
        self.manager.stage(archive, checksum, apply=True)
        return manifest["release"]

    def test_drain_timeout_leaves_service_running_and_current_unchanged(self):
        original = self.adopted()
        candidate = self.staged()
        service = FakeService(running=True)
        api = FakeMaintenanceAPI(active=1)
        maintenance = Maintenance(api.request, wait=0, state_file=self.manager.root / ".maintenance-operation.json")
        with self.assertRaisesRegex(RuntimeError, "wait expired"):
            self.manager.activate(candidate, service, lambda _: None, apply=True, maintenance=maintenance)
        self.assertEqual(self.manager.target("current"), original)
        self.assertTrue(service.running)
        self.assertEqual(service.events, [])
        self.assertFalse(api.draining)
        self.assertFalse(maintenance.state_file.exists())

    def test_drain_rollback_to_unsupported_release_is_refused_before_mutation(self):
        self.adopted()
        current = self.staged()
        self.manager.activate(current, FakeService(), lambda _: None, apply=True)
        service = FakeService(running=True)
        api = FakeMaintenanceAPI()
        with self.assertRaisesRegex(ValueError, "does not declare durable maintenance"):
            self.manager.activate(None, service, lambda _: None, apply=True, maintenance=Maintenance(api.request))
        self.assertEqual(self.manager.target("current"), current)
        self.assertEqual(service.events, [])
        self.assertEqual(api.methods, [])

    def test_same_release_with_closed_admission_requires_explicit_recovery(self):
        self.adopted()
        current = self.staged()
        self.manager.activate(current, FakeService(), lambda _: None, apply=True)
        service = FakeService(running=True)
        api = FakeMaintenanceAPI()
        api.draining = True
        with self.assertRaisesRegex(ValueError, "admission is closed"):
            self.manager.activate(current, service, lambda _: None, apply=True, maintenance=Maintenance(api.request))
        self.assertEqual(service.events, [])

    def test_saved_maintenance_cannot_be_bypassed_with_stopped_service_mode(self):
        original = self.adopted()
        candidate = self.staged()
        (self.manager.root / ".maintenance-operation.json").write_text("saved-operation")
        with self.assertRaisesRegex(ValueError, "saved maintenance"):
            self.manager.activate(candidate, FakeService(), lambda _: None, apply=True)
        self.assertEqual(self.manager.target("current"), original)

    def test_drain_precedes_stop_and_switch_and_plan_does_not_mutate(self):
        self.adopted()
        candidate = self.staged()
        service = FakeService(running=True)
        api = FakeMaintenanceAPI(active=0)
        maintenance = Maintenance(api.request, wait=0, state_file=self.manager.root / ".maintenance-operation.json")
        self.manager.activate(candidate, service, lambda _: None, maintenance=maintenance)
        self.assertFalse(api.draining)
        self.assertEqual(service.events, [])
        stop = service.stop

        def drained_stop():
            self.assertTrue(api.draining)
            self.assertEqual(api.active, 0)
            self.assertEqual(maintenance.state_file.stat().st_mode & 0o777, 0o600)
            stop()

        service.stop = drained_stop
        self.manager.activate(candidate, service, lambda _: None, apply=True, maintenance=maintenance)
        self.assertEqual(service.events, ["stop", "start"])
        self.assertEqual(self.manager.target("current"), candidate)
        self.assertFalse(maintenance.state_file.exists())

    def test_unhealthy_candidate_with_work_is_not_forcibly_stopped(self):
        self.adopted()
        candidate = self.staged()
        service = FakeService(running=True)
        api = FakeMaintenanceAPI(active=0)
        maintenance = Maintenance(api.request, wait=0)

        def health(manifest):
            if manifest["release"] == candidate:
                api.draining = False  # The new process starts with open admission.
                api.active = 1
                api.instance = "candidate"
                raise RuntimeError("candidate health failed")

        with self.assertRaisesRegex(RuntimeError, "wait expired"):
            self.manager.activate(candidate, service, health, apply=True, maintenance=maintenance)
        self.assertEqual(service.events, ["stop", "start"])
        self.assertEqual(self.manager.target("current"), candidate)
        self.assertTrue(service.running)

    def test_unhealthy_candidate_drains_before_automatic_rollback(self):
        original = self.adopted()
        candidate = self.staged()
        service = FakeService(running=True)
        api = FakeMaintenanceAPI(active=0)
        maintenance = Maintenance(api.request, wait=0)

        def health(manifest):
            if manifest["release"] == candidate:
                api.draining = False
                api.instance = "candidate"
                raise RuntimeError("candidate health failed")

        with self.assertRaisesRegex(RuntimeError, "previous release restored and healthy"):
            self.manager.activate(candidate, service, health, apply=True, maintenance=maintenance)
        self.assertEqual(service.events, ["stop", "start", "stop", "start"])
        self.assertEqual(self.manager.target("current"), original)

    def test_failed_link_after_drain_restarts_and_resumes_original_release(self):
        original = self.adopted()
        candidate = self.staged()
        service = FakeService(running=True)
        api = FakeMaintenanceAPI()
        maintenance = Maintenance(api.request, wait=0, state_file=self.manager.root / ".maintenance-operation.json")
        link = self.manager.link

        def fail_candidate_link(name, release):
            if name == "current" and release == candidate:
                raise OSError("simulated failed atomic switch")
            link(name, release)

        self.manager.link = fail_candidate_link
        with self.assertRaisesRegex(RuntimeError, "previous release restored and healthy"):
            self.manager.activate(candidate, service, lambda _: None, apply=True, maintenance=maintenance)
        self.assertEqual(self.manager.target("current"), original)
        self.assertTrue(service.running)
        self.assertFalse(api.draining)
        self.assertFalse(maintenance.state_file.exists())

    def test_resume_failure_after_healthy_candidate_does_not_roll_back(self):
        self.adopted()
        candidate = self.staged()
        service = FakeService(running=True)
        api = FakeMaintenanceAPI()
        operation = Maintenance(api.request, wait=0, state_file=self.manager.root / ".maintenance-operation.json")

        def health(manifest):
            if manifest["release"] == candidate:
                api.fail_resume = True

        with self.assertRaisesRegex(RuntimeError, "recovery required"):
            self.manager.activate(candidate, service, health, apply=True, maintenance=operation)
        self.assertEqual(self.manager.target("current"), candidate)
        self.assertEqual(service.events, ["stop", "start"])
        self.assertTrue(operation.state_file.exists())
        self.assertTrue(api.draining)
        api.fail_resume = False
        recovery = Maintenance(api.request, state_file=operation.state_file)
        recovery.load()
        recovery.resume()
        self.assertFalse(api.draining)

    def test_plan_verifies_archive_without_creating_root(self):
        archive, checksum, _ = self.archive()
        result = self.manager.stage(archive, checksum)
        self.assertFalse(result["apply"])
        self.assertFalse(self.manager.root.exists())

    def test_adoption_rejects_abbreviated_provenance_before_writing_state(self):
        binary = self.directory / "existing-binary"
        binary.write_bytes(executable("original"))
        with self.assertRaisesRegex(ValueError, "Invalid commit"):
            self.manager.adopt(binary, hashlib.sha256(binary.read_bytes()).hexdigest(),
                               "8.0.29", "b" * 7, apply=True)
        self.assertFalse(self.manager.root.exists())

    def test_bad_archive_checksum_preserves_current_release(self):
        original = self.adopted()
        archive, _, _ = self.archive()
        with self.assertRaisesRegex(ValueError, "Archive checksum"):
            self.manager.stage(archive, "0" * 64, apply=True)
        self.assertEqual(self.manager.target("current"), original)
        self.assertEqual(len(list((self.manager.root / "releases").iterdir())), 1)

    def test_archive_rejects_traversal_symlinks_and_duplicate_members(self):
        for name, kind in (("../escape", tarfile.REGTYPE), ("cli-proxy-api", tarfile.SYMTYPE), ("manifest.json", tarfile.REGTYPE)):
            with self.subTest(name=name, kind=kind):
                entry = tarfile.TarInfo(name)
                entry.type = kind
                entry.linkname = "/tmp/escape"
                archive, checksum, _ = self.archive(extra=entry)
                with self.assertRaisesRegex(ValueError, "only regular"):
                    self.manager.stage(archive, checksum, apply=True)
                self.assertFalse((self.directory / "escape").exists())

    def test_non_linux_binary_is_rejected(self):
        archive, checksum, _ = self.archive(binary=b"#!/bin/sh\necho wrong platform\n")
        with self.assertRaisesRegex(ValueError, "ELF executable"):
            self.manager.stage(archive, checksum, apply=True)

    def test_stage_is_idempotent_and_release_identifiers_are_immutable(self):
        archive, checksum, _ = self.archive()
        self.assertTrue(self.manager.stage(archive, checksum, apply=True)["changed"])
        self.assertFalse(self.manager.stage(archive, checksum, apply=True)["changed"])
        archive, checksum, _ = self.archive(binary=executable("different"))
        with self.assertRaisesRegex(ValueError, "immutable release"):
            self.manager.stage(archive, checksum, apply=True)

    def test_concurrent_operation_is_rejected(self):
        archive, checksum, _ = self.archive()
        with self.manager.transaction(True):
            with self.assertRaisesRegex(ValueError, "Another release"):
                self.manager.stage(archive, checksum, apply=True)

    def test_group_writable_umask_does_not_make_release_metadata_unusable(self):
        previous_umask = os.umask(0o002)
        try:
            archive, checksum, _ = self.archive()
            self.manager.stage(archive, checksum, apply=True)
            self.assertFalse(self.manager.stage(archive, checksum, apply=True)["changed"])
        finally:
            os.umask(previous_umask)

    def test_activation_refuses_running_service_without_changing_links(self):
        original = self.adopted()
        candidate = self.staged()
        service = FakeService(running=True)
        with self.assertRaisesRegex(ValueError, "Service is active"):
            self.manager.activate(candidate, service, lambda _: None, apply=True)
        self.assertEqual(self.manager.target("current"), original)
        self.assertEqual(service.events, [])

    def test_successful_activation_keeps_previous_binary_for_rollback(self):
        original = self.adopted()
        candidate = self.staged()
        service = FakeService()
        checked = []
        self.manager.activate(candidate, service, lambda manifest: checked.append(manifest["release"]), apply=True)
        self.assertEqual(self.manager.target("current"), candidate)
        self.assertEqual(self.manager.target("previous"), original)
        self.assertEqual(service.events, ["start"])
        self.assertEqual(checked, [candidate])
        service.stop()  # The operator drains/stops work before an explicit rollback too.
        self.manager.activate(self.manager.target("previous"), service, lambda _: None, apply=True)
        self.assertEqual(self.manager.target("current"), original)

    def test_unhealthy_candidate_restores_binary_without_restoring_oauth(self):
        older = self.adopted()
        original = self.staged()
        service = FakeService()
        self.manager.activate(original, service, lambda _: None, apply=True)
        service.stop()
        service.events.clear()
        candidate = self.staged("8.0.29-fleet.2")
        token = self.directory / "oauth.json"
        token.write_text("original token")

        def health(manifest):
            if manifest["release"] == candidate:
                token.write_text("refreshed token")
                raise RuntimeError("unhealthy candidate")

        with self.assertRaisesRegex(RuntimeError, "previous release restored and healthy"):
            self.manager.activate(candidate, service, health, apply=True)
        self.assertEqual(self.manager.target("current"), original)
        self.assertEqual(self.manager.target("previous"), older)
        self.assertEqual(token.read_text(), "refreshed token")
        self.assertEqual(service.events, ["start", "stop", "start"])

        service.stop()
        def unhealthy_restore(manifest):
            health(manifest)
            raise RuntimeError("unhealthy restore")

        with self.assertRaisesRegex(RuntimeError, "unhealthy restore"):
            self.manager.activate(candidate, service, unhealthy_restore, apply=True)
        self.assertEqual(self.manager.target("current"), original)
        self.assertEqual(self.manager.target("previous"), older)

    def test_failure_to_stop_candidate_does_not_repoint_its_executable(self):
        original = self.adopted()
        candidate = self.staged()
        service = FakeService()
        service.stop = lambda: (_ for _ in ()).throw(RuntimeError("stop failed"))
        with self.assertRaisesRegex(RuntimeError, "stop failed"):
            self.manager.activate(candidate, service, lambda _: (_ for _ in ()).throw(RuntimeError()), apply=True)
        self.assertEqual(self.manager.target("current"), candidate)
        self.assertEqual(self.manager.target("previous"), original)

    def test_interrupted_activation_restores_previous_release(self):
        original = self.adopted()
        candidate = self.staged()
        service = FakeService()

        def health(manifest):
            if manifest["release"] == candidate:
                raise KeyboardInterrupt()

        with self.assertRaisesRegex(RuntimeError, "previous release restored and healthy"):
            self.manager.activate(candidate, service, health, apply=True)
        self.assertEqual(self.manager.target("current"), original)
        self.assertTrue(service.running)

    def test_installed_binary_corruption_is_detected_before_switch(self):
        original = self.adopted()
        candidate = self.staged()
        (self.manager.root / "releases" / candidate / "cli-proxy-api").write_bytes(executable("tampered"))
        service = FakeService()
        with self.assertRaisesRegex(ValueError, "Binary checksum"):
            self.manager.activate(candidate, service, lambda _: None, apply=True)
        self.assertEqual(self.manager.target("current"), original)
        self.assertEqual(service.events, [])

    def test_same_release_does_not_claim_success_without_health(self):
        original = self.adopted()
        service = FakeService()
        with self.assertRaisesRegex(RuntimeError, "not serving"):
            self.manager.activate(original, service, lambda _: (_ for _ in ()).throw(RuntimeError("not serving")), apply=True)
        self.assertEqual(service.events, [])

    def test_systemd_checks_exact_service_and_executable(self):
        with patch("fork_release.sys.platform", "linux"), patch("fork_release.platform.machine", return_value="x86_64"), patch("fork_release.os.getuid", return_value=1000):
            service = UserService("proxy.service")
        executable_path = self.manager.root / "current" / "cli-proxy-api"
        service.run = lambda *_: f"LoadState=loaded\nActiveState=inactive\nMainPID=0\nControlPID=0\nExecStart={{ path={executable_path} ; argv[]={executable_path} --config config.yaml ; }}\n"
        service.require_stopped(executable_path)
        with self.assertRaisesRegex(ValueError, "ExecStart"):
            service.require_stopped(self.directory / "other-binary")

    def test_failed_systemd_candidate_can_be_replaced_by_the_previous_binary(self):
        original = self.adopted()
        candidate = self.staged()
        with patch("fork_release.sys.platform", "linux"), patch("fork_release.platform.machine", return_value="x86_64"), patch("fork_release.os.getuid", return_value=1000):
            service = UserService("proxy.service")
        state = "inactive"
        starts = 0
        resets = 0

        def run(*args):
            nonlocal state, starts, resets
            if args[0] == "show":
                path = self.manager.root / "current" / "cli-proxy-api"
                return f"LoadState=loaded\nActiveState={state}\nMainPID=0\nControlPID=0\nExecStart={{ path={path} ; }}\n"
            if args[0] == "reset-failed":
                resets += 1
                state = "inactive"
            elif args[0] == "start":
                starts += 1
                state = "failed" if starts == 1 else "active"
            elif args[0] == "stop" and state != "failed":
                state = "inactive"
            return ""

        def health(manifest):
            if manifest["release"] == candidate:
                raise RuntimeError("candidate crashed")

        service.run = run
        with self.assertRaisesRegex(RuntimeError, "previous release restored and healthy"):
            self.manager.activate(candidate, service, health, apply=True)
        self.assertEqual(self.manager.target("current"), original)
        self.assertEqual(state, "active")
        self.assertEqual(resets, 2)

    def test_health_rejects_remote_origins_and_unprotected_key_files(self):
        key = self.directory / "management.key"
        key.write_text("test-only-key")
        key.chmod(0o644)
        with self.assertRaisesRegex(ValueError, "loopback"):
            management_health("https://example.com", key)
        with self.assertRaisesRegex(ValueError, "private"):
            management_health("http://127.0.0.1:18317", key)

    def test_health_does_not_report_closed_admission_as_normal_success(self):
        _, _, manifest = self.archive(maintenance=True)
        api = FakeMaintenanceAPI()
        api.draining = True

        def request(path, method="GET", data=None):
            response = api.request(path, method, data) if path == Maintenance.path else io.BytesIO()
            response.headers = {"X-CPA-VERSION": manifest["version"], "X-CPA-COMMIT": manifest["commit"]}
            return response

        with patch("fork_release.management_request", return_value=request):
            with self.assertRaisesRegex(RuntimeError, "admission check failed"):
                management_health("unused", "unused", wait=0)(manifest)
            management_health("unused", "unused", wait=0, allow_draining=True)(manifest)
            api.draining = False
            management_health("unused", "unused", wait=0)(manifest)
            api.supported = False
            api.durable = False
            management_health("unused", "unused", wait=0)(manifest)

    def test_health_authenticates_checks_build_and_refuses_redirects(self):
        _, _, manifest = self.archive()
        key = self.directory / "management.key"
        key.write_text("test-only-key")
        key.chmod(0o600)
        received = []

        class Handler(http.server.BaseHTTPRequestHandler):
            version_header = manifest["version"]
            redirect = False

            def log_message(self, *args):
                pass

            def do_GET(self):
                received.append((self.path, self.headers.get("Authorization")))
                self.send_response(302 if self.redirect else 200)
                self.send_header("Location", "/must-not-receive-key")
                self.send_header("X-CPA-VERSION", self.version_header)
                self.send_header("X-CPA-COMMIT", manifest["commit"][:8])
                self.end_headers()

        server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        thread.start()
        try:
            check = management_health(f"http://127.0.0.1:{server.server_port}", key, wait=0)
            check(manifest)
            self.assertEqual(received, [("/v8/management/config", "Bearer test-only-key")])
            Handler.version_header = "wrong-build"
            with self.assertRaisesRegex(RuntimeError, "health/version"):
                check(manifest)
            Handler.redirect = True
            with self.assertRaisesRegex(RuntimeError, "health/version"):
                check(manifest)
            self.assertTrue(all(path == "/v8/management/config" for path, _ in received))
        finally:
            server.shutdown()
            thread.join()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
